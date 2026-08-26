package relayserver

import (
	"context"
	"crypto/ed25519"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/fileprotocol"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

const (
	maxFileSessionsPerDevice        = 1
	maxFileDuration                 = 15 * time.Minute
	maxFileTicketBytes       uint64 = 16 << 20
	fileFenceRefresh                = 30 * time.Second
)

var ErrFileFrameInvalid = errors.New("File frame is invalid")

type FileRouteRegistry interface {
	ConsumeFileTicket(context.Context, string, time.Time, time.Time) error
}

type FileForwarderConfig struct {
	NodeID   string
	CellID   string
	Verifier TicketVerifier
	// Devices is deprecated. Identity and grant versions are authorized by Host
	// and embedded in the signed, short-lived File Ticket.
	Devices     PeerDeviceStateResolver
	Routes      FileRouteRegistry
	Connections *ConnectionManager
	Now         func() time.Time
}

type FileForwarder struct {
	nodeID      string
	cellID      string
	verifier    TicketVerifier
	routes      FileRouteRegistry
	connections *ConnectionManager
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]*fileSession
}

type fileSession struct {
	id                    string
	ticketJWTID           string
	sourceDeviceID        string
	targetDeviceID        string
	sourceEpoch           uint64
	targetEpoch           uint64
	sourceGrantVersion    uint64
	targetGrantVersion    uint64
	sourceKeyThumbprint   string
	targetKeyThumbprint   string
	sourceIdentityKey     ed25519.PublicKey
	targetIdentityKey     ed25519.PublicKey
	generation            uint64
	state                 relayprotocol.TransferState
	expiresAt             time.Time
	lastFenceAt           time.Time
	maximumForwardedBytes uint64
	forwardedBytes        uint64
	lastSequences         map[string]uint64
	open                  fileprotocol.OpenTranscript
	openSignature         []byte
}

func NewFileForwarder(config FileForwarderConfig) (*FileForwarder, error) {
	if config.NodeID == "" || config.CellID == "" || config.Verifier == nil || config.Routes == nil || config.Connections == nil {
		return nil, errors.New("File forwarder dependencies are required")
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &FileForwarder{
		nodeID: config.NodeID, cellID: config.CellID, verifier: config.Verifier,
		routes: config.Routes, connections: config.Connections, now: now, sessions: make(map[string]*fileSession),
	}, nil
}

func (forwarder *FileForwarder) HandleFromDevice(ctx context.Context, deviceID string, epoch uint64, envelope *remotev1.Envelope) error {
	if ctx.Err() != nil || deviceID == "" || epoch == 0 || envelope == nil || envelope.GetConnectionEpoch() != epoch {
		return ErrFileFrameInvalid
	}
	if open := envelope.GetFileOpen(); open != nil {
		return forwarder.handleOpen(ctx, deviceID, epoch, open)
	}
	if accept := envelope.GetFileAccept(); accept != nil {
		return forwarder.handleAccept(ctx, deviceID, epoch, envelope, accept)
	}
	if reject := envelope.GetFileReject(); reject != nil {
		return forwarder.handleReject(deviceID, epoch, envelope, reject)
	}
	return forwarder.handlePayload(ctx, deviceID, epoch, envelope)
}

func (forwarder *FileForwarder) handleOpen(ctx context.Context, sourceDeviceID string, sourceEpoch uint64, open *remotev1.FileOpen) error {
	now := forwarder.now().UTC()
	claims, sourceKey, targetKey, code, err := forwarder.validateOpen(ctx, sourceDeviceID, open, now)
	if err != nil {
		return forwarder.sendReject(sourceDeviceID, sourceEpoch, open.GetTransferId(), code, code == remotev1.ErrorCode_ERROR_CODE_FILE_TARGET_OFFLINE)
	}
	if err := forwarder.routes.ConsumeFileTicket(ctx, claims.JWTID, time.Unix(claims.ExpiresAt, 0).UTC(), now); err != nil {
		return forwarder.sendReject(sourceDeviceID, sourceEpoch, claims.TransferID, remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_INVALID, false)
	}
	maximumForwarded := claims.MaxBytes
	for _, overhead := range []uint64{uint64(claims.MaxManifestBytes), uint64(claims.MaxFileCount) * 32, 4 << 20} {
		if maximumForwarded > math.MaxUint64-overhead {
			maximumForwarded = math.MaxUint64
			break
		}
		maximumForwarded += overhead
	}
	transcript := fileOpenTranscript(claims, open)
	session := &fileSession{
		id: claims.TransferID, ticketJWTID: claims.JWTID, sourceDeviceID: claims.SourceDeviceID, targetDeviceID: claims.TargetDeviceID,
		sourceEpoch: sourceEpoch, targetEpoch: claims.TargetConnectionEpoch,
		sourceGrantVersion: claims.SourceGrantVersion, targetGrantVersion: claims.TargetGrantVersion,
		sourceKeyThumbprint: claims.SourceKeyThumbprint, targetKeyThumbprint: claims.TargetKeyThumbprint,
		sourceIdentityKey: append(ed25519.PublicKey(nil), sourceKey...), targetIdentityKey: append(ed25519.PublicKey(nil), targetKey...),
		generation: open.GetGeneration(), state: relayprotocol.TransferOffered,
		expiresAt: now.Add(time.Duration(claims.MaxDurationSeconds) * time.Second), lastFenceAt: now,
		maximumForwardedBytes: maximumForwarded, lastSequences: make(map[string]uint64),
		open: transcript, openSignature: append([]byte(nil), open.GetIdentitySignature()...),
	}
	if !forwarder.registerSession(session) {
		return forwarder.sendReject(sourceDeviceID, sourceEpoch, claims.TransferID, remotev1.ErrorCode_ERROR_CODE_FILE_RATE_LIMITED, true)
	}
	if err := forwarder.forward(session.targetDeviceID, session.targetEpoch, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: sourceEpoch, Frame: &remotev1.Envelope_FileOpen{FileOpen: open},
	}); err != nil {
		forwarder.deleteSession(session.id)
		return forwarder.sendReject(sourceDeviceID, sourceEpoch, claims.TransferID, remotev1.ErrorCode_ERROR_CODE_FILE_TARGET_OFFLINE, true)
	}
	return nil
}

func (forwarder *FileForwarder) validateOpen(_ context.Context, sourceDeviceID string, open *remotev1.FileOpen, now time.Time) (remoteauth.Claims, ed25519.PublicKey, ed25519.PublicKey, remotev1.ErrorCode, error) {
	invalid := func(code remotev1.ErrorCode) (remoteauth.Claims, ed25519.PublicKey, ed25519.PublicKey, remotev1.ErrorCode, error) {
		return remoteauth.Claims{}, nil, nil, code, ErrFileFrameInvalid
	}
	if open == nil || len(open.GetFileTicket()) < 32 || len(open.GetFileTicket()) > 16<<10 || uuid.Validate(open.GetTransferId()) != nil ||
		open.GetGeneration() == 0 || len(open.GetEphemeralPublicKey()) != fileprotocol.EphemeralPublicKeySize ||
		len(open.GetIdentitySignature()) != ed25519.SignatureSize || open.GetDeclaredTotalBytes() == 0 || open.GetDeclaredFileCount() == 0 {
		return invalid(remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_INVALID)
	}
	claims, err := forwarder.verifier.Verify(open.GetFileTicket(), "relay-file", now)
	if err != nil {
		code := remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_INVALID
		if errors.Is(err, remoteauth.ErrTicketExpired) {
			code = remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_EXPIRED
		}
		return invalid(code)
	}
	if claims.TransferID != open.GetTransferId() || claims.SourceDeviceID != sourceDeviceID || claims.SourceDeviceID == claims.TargetDeviceID ||
		claims.MaxDurationSeconds == 0 || time.Duration(claims.MaxDurationSeconds)*time.Second > maxFileDuration ||
		claims.MaxBytes == 0 || claims.MaxBytes > maxFileTicketBytes || claims.MaxFileCount == 0 || claims.MaxFileCount > 500 ||
		claims.MaxManifestBytes == 0 || claims.MaxManifestBytes > 1<<20 || claims.AllowedChunkSize != fileprotocol.DefaultChunkSize ||
		open.GetDeclaredTotalBytes() > claims.MaxBytes || open.GetDeclaredFileCount() > claims.MaxFileCount || !claims.RequireLocalApproval ||
		claims.ValidateFile(claims.SourceDeviceID, claims.TargetDeviceID, claims.SourceKeyThumbprint, claims.TargetKeyThumbprint,
			claims.SourceGrantVersion, claims.TargetGrantVersion) != nil ||
		claims.ValidateFileRelay(forwarder.nodeID, forwarder.cellID, claims.TargetConnectionEpoch) != nil {
		return invalid(remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_INVALID)
	}
	claimedSourceKey, err := remoteauth.DecodeIdentityPublicKey(claims.SourceIdentityKey, claims.SourceKeyThumbprint)
	if err != nil {
		return invalid(remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_INVALID)
	}
	claimedTargetKey, err := remoteauth.DecodeIdentityPublicKey(claims.TargetIdentityKey, claims.TargetKeyThumbprint)
	if err != nil {
		return invalid(remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_INVALID)
	}
	if !forwarder.connections.HasResident(claims.TargetDeviceID, claims.TargetConnectionEpoch) {
		return invalid(remotev1.ErrorCode_ERROR_CODE_FILE_TARGET_OFFLINE)
	}
	if err := fileprotocol.VerifyOpen(claimedSourceKey, fileOpenTranscript(claims, open), open.GetIdentitySignature()); err != nil {
		return invalid(remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_INVALID)
	}
	return claims, claimedSourceKey, claimedTargetKey, remotev1.ErrorCode_ERROR_CODE_UNSPECIFIED, nil
}

func fileOpenTranscript(claims remoteauth.Claims, open *remotev1.FileOpen) fileprotocol.OpenTranscript {
	return fileprotocol.OpenTranscript{
		TicketJWTID: claims.JWTID, TransferID: claims.TransferID, Generation: open.GetGeneration(),
		SourceDeviceID: claims.SourceDeviceID, TargetDeviceID: claims.TargetDeviceID,
		SourceEphemeralPublicKey: append([]byte(nil), open.GetEphemeralPublicKey()...),
		DeclaredTotalBytes:       open.GetDeclaredTotalBytes(), DeclaredFileCount: open.GetDeclaredFileCount(),
	}
}

func (forwarder *FileForwarder) handleAccept(ctx context.Context, deviceID string, epoch uint64, envelope *remotev1.Envelope, accept *remotev1.FileAccept) error {
	now := forwarder.now().UTC()
	forwarder.mu.Lock()
	session := forwarder.sessions[accept.GetTransferId()]
	if session == nil || deviceID != session.targetDeviceID || epoch != session.targetEpoch || accept.GetGeneration() != session.generation ||
		session.state != relayprotocol.TransferOffered || len(accept.GetEphemeralPublicKey()) != fileprotocol.EphemeralPublicKeySize ||
		len(accept.GetIdentitySignature()) != ed25519.SignatureSize || accept.GetCipherSuite() != remotev1.FileCipherSuite_FILE_CIPHER_SUITE_XCHACHA20_POLY1305 ||
		accept.GetChunkSizeBytes() != fileprotocol.DefaultChunkSize || accept.GetReceiveWindowBytes() < accept.GetChunkSizeBytes() || accept.GetReceiveWindowBytes() > 4<<20 ||
		!session.expiresAt.After(now) {
		forwarder.mu.Unlock()
		return ErrFileFrameInvalid
	}
	acceptTranscript := fileprotocol.AcceptTranscript{
		TargetEphemeralPublicKey: accept.GetEphemeralPublicKey(), CipherSuite: fileprotocol.XChaCha20Poly1305,
		ChunkSize: accept.GetChunkSizeBytes(), ReceiveWindow: accept.GetReceiveWindowBytes(),
	}
	if fileprotocol.VerifyAccept(session.targetIdentityKey, session.open, session.openSignature, acceptTranscript, accept.GetIdentitySignature()) != nil {
		forwarder.mu.Unlock()
		return ErrFileFrameInvalid
	}
	session.state = relayprotocol.TransferAccepted
	targetID, targetEpoch := session.sourceDeviceID, session.sourceEpoch
	forwarder.mu.Unlock()
	if err := forwarder.refreshFence(ctx, session, now); err != nil {
		forwarder.deleteSession(session.id)
		return forwarder.sendFileError(deviceID, epoch, session, remotev1.ErrorCode_ERROR_CODE_FILE_GRANT_REVOKED, false)
	}
	return forwarder.forward(targetID, targetEpoch, envelope)
}

func (forwarder *FileForwarder) handleReject(deviceID string, epoch uint64, envelope *remotev1.Envelope, reject *remotev1.FileReject) error {
	forwarder.mu.Lock()
	session := forwarder.sessions[reject.GetTransferId()]
	if session == nil || deviceID != session.targetDeviceID || epoch != session.targetEpoch || session.state != relayprotocol.TransferOffered ||
		reject.GetCode() == remotev1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		forwarder.mu.Unlock()
		return ErrFileFrameInvalid
	}
	delete(forwarder.sessions, session.id)
	targetID, targetEpoch := session.sourceDeviceID, session.sourceEpoch
	forwarder.mu.Unlock()
	return forwarder.forward(targetID, targetEpoch, envelope)
}

func (forwarder *FileForwarder) handlePayload(ctx context.Context, deviceID string, epoch uint64, envelope *remotev1.Envelope) error {
	frameType, class := envelopeClass(envelope)
	if class != relayprotocol.FrameClassFilePayload {
		return ErrFileFrameInvalid
	}
	transferID, generation, sequence, ciphertextBytes, terminal, err := filePayloadMetadata(envelope)
	if err != nil {
		return err
	}
	now := forwarder.now().UTC()
	forwarder.mu.Lock()
	session := forwarder.sessions[transferID]
	isSource := session != nil && deviceID == session.sourceDeviceID && epoch == session.sourceEpoch
	isTarget := session != nil && deviceID == session.targetDeviceID && epoch == session.targetEpoch
	if session == nil || (!isSource && !isTarget) || generation != session.generation || !session.expiresAt.After(now) ||
		!validFileFrameDirection(frameType, isSource) || (session.state != relayprotocol.TransferAccepted && session.state != relayprotocol.TransferTransferring && session.state != relayprotocol.TransferVerifying) {
		forwarder.mu.Unlock()
		return ErrFileFrameInvalid
	}
	wanted := session.lastSequences[deviceID] + 1
	if sequence > 0 && sequence != wanted {
		forwarder.mu.Unlock()
		return ErrFileFrameInvalid
	}
	if ciphertextBytes > session.maximumForwardedBytes-session.forwardedBytes {
		forwarder.mu.Unlock()
		return forwarder.sendFileError(deviceID, epoch, session, remotev1.ErrorCode_ERROR_CODE_FILE_QUOTA_EXCEEDED, false)
	}
	if sequence > 0 {
		session.lastSequences[deviceID] = wanted
	}
	session.forwardedBytes += ciphertextBytes
	previousState := session.state
	if session.state == relayprotocol.TransferAccepted {
		session.state = relayprotocol.TransferTransferring
	}
	if frameType == "FILE_COMPLETE" {
		if !isSource || previousState != relayprotocol.TransferTransferring {
			forwarder.mu.Unlock()
			return ErrFileFrameInvalid
		}
		session.state = relayprotocol.TransferVerifying
	}
	if frameType == "FILE_VERIFIED" && (!isTarget || previousState != relayprotocol.TransferVerifying) {
		forwarder.mu.Unlock()
		return ErrFileFrameInvalid
	}
	peerID, peerEpoch := session.sourceDeviceID, session.sourceEpoch
	if isSource {
		peerID, peerEpoch = session.targetDeviceID, session.targetEpoch
	}
	forwarder.mu.Unlock()
	if err := forwarder.refreshFence(ctx, session, now); err != nil {
		forwarder.deleteSession(session.id)
		return forwarder.sendFileError(deviceID, epoch, session, remotev1.ErrorCode_ERROR_CODE_FILE_GRANT_REVOKED, false)
	}
	if err := forwarder.forward(peerID, peerEpoch, envelope); err != nil {
		forwarder.deleteSession(session.id)
		return forwarder.sendFileError(deviceID, epoch, session, remotev1.ErrorCode_ERROR_CODE_FILE_INTERRUPTED, true)
	}
	if terminal {
		forwarder.deleteSession(session.id)
	}
	return nil
}

func filePayloadMetadata(envelope *remotev1.Envelope) (string, uint64, uint64, uint64, bool, error) {
	if fileError := envelope.GetFileError(); fileError != nil {
		if uuid.Validate(fileError.GetTransferId()) != nil || fileError.GetGeneration() == 0 || fileError.GetCode() == remotev1.ErrorCode_ERROR_CODE_UNSPECIFIED ||
			len(fileError.GetEncryptedDetail()) > relayprotocol.ControlFrameLimit {
			return "", 0, 0, 0, false, ErrFileFrameInvalid
		}
		return fileError.GetTransferId(), fileError.GetGeneration(), 0, uint64(len(fileError.GetEncryptedDetail())), true, nil
	}
	var ciphertext *remotev1.FileCiphertext
	switch {
	case envelope.GetFileManifest() != nil:
		ciphertext = envelope.GetFileManifest()
	case envelope.GetFileChunk() != nil:
		ciphertext = envelope.GetFileChunk()
	case envelope.GetFileAck() != nil:
		ciphertext = envelope.GetFileAck()
	case envelope.GetFileWindowUpdate() != nil:
		ciphertext = envelope.GetFileWindowUpdate()
	case envelope.GetFileResume() != nil:
		ciphertext = envelope.GetFileResume()
	case envelope.GetFileComplete() != nil:
		ciphertext = envelope.GetFileComplete()
	case envelope.GetFileVerified() != nil:
		ciphertext = envelope.GetFileVerified()
	case envelope.GetFileCancel() != nil:
		ciphertext = envelope.GetFileCancel()
	default:
		return "", 0, 0, 0, false, ErrFileFrameInvalid
	}
	if uuid.Validate(ciphertext.GetTransferId()) != nil || ciphertext.GetGeneration() == 0 || ciphertext.GetMessageSequence() == 0 ||
		len(ciphertext.GetCiphertext()) == 0 || len(ciphertext.GetCiphertext()) > relayprotocol.AbsoluteFrameLimit {
		return "", 0, 0, 0, false, ErrFileFrameInvalid
	}
	terminal := envelope.GetFileVerified() != nil || envelope.GetFileCancel() != nil
	return ciphertext.GetTransferId(), ciphertext.GetGeneration(), ciphertext.GetMessageSequence(), uint64(len(ciphertext.GetCiphertext())), terminal, nil
}

func validFileFrameDirection(frameType string, isSource bool) bool {
	if isSource {
		return frameType == "FILE_MANIFEST" || frameType == "FILE_CHUNK" || frameType == "FILE_RESUME" ||
			frameType == "FILE_COMPLETE" || frameType == "FILE_CANCEL" || frameType == "FILE_ERROR"
	}
	return frameType == "FILE_ACK" || frameType == "FILE_WINDOW_UPDATE" || frameType == "FILE_RESUME" ||
		frameType == "FILE_VERIFIED" || frameType == "FILE_CANCEL" || frameType == "FILE_ERROR"
}

func (forwarder *FileForwarder) registerSession(candidate *fileSession) bool {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	forwarder.removeExpiredLocked(forwarder.now().UTC())
	if current := forwarder.sessions[candidate.id]; current != nil {
		if current.sourceDeviceID != candidate.sourceDeviceID || current.targetDeviceID != candidate.targetDeviceID || candidate.generation <= current.generation {
			return false
		}
		delete(forwarder.sessions, candidate.id)
	}
	if forwarder.sessionCountLocked(candidate.sourceDeviceID) >= maxFileSessionsPerDevice ||
		forwarder.sessionCountLocked(candidate.targetDeviceID) >= maxFileSessionsPerDevice {
		return false
	}
	forwarder.sessions[candidate.id] = candidate
	return true
}

func (forwarder *FileForwarder) refreshFence(_ context.Context, session *fileSession, now time.Time) error {
	forwarder.mu.Lock()
	if now.Sub(session.lastFenceAt) < fileFenceRefresh {
		forwarder.mu.Unlock()
		return nil
	}
	forwarder.mu.Unlock()
	if !forwarder.connections.HasResident(session.sourceDeviceID, session.sourceEpoch) ||
		!forwarder.connections.HasResident(session.targetDeviceID, session.targetEpoch) {
		return ErrFileFrameInvalid
	}
	forwarder.mu.Lock()
	if current := forwarder.sessions[session.id]; current == session {
		session.lastFenceAt = now
	}
	forwarder.mu.Unlock()
	return nil
}

func (forwarder *FileForwarder) forward(targetID string, targetEpoch uint64, envelope *remotev1.Envelope) error {
	copy, ok := proto.Clone(envelope).(*remotev1.Envelope)
	if !ok || targetID == "" || targetEpoch == 0 {
		return ErrFileFrameInvalid
	}
	copy.ConnectionEpoch = targetEpoch
	return forwarder.connections.EnqueueEndpoint(targetID, targetEpoch, copy)
}

func (forwarder *FileForwarder) sendReject(deviceID string, epoch uint64, transferID string, code remotev1.ErrorCode, retryable bool) error {
	if deviceID == "" || epoch == 0 || code == remotev1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		return ErrFileFrameInvalid
	}
	return forwarder.connections.EnqueueEndpoint(deviceID, epoch, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: epoch,
		Frame: &remotev1.Envelope_FileReject{FileReject: &remotev1.FileReject{TransferId: transferID, Code: code, Retryable: retryable}},
	})
}

func (forwarder *FileForwarder) sendFileError(deviceID string, epoch uint64, session *fileSession, code remotev1.ErrorCode, retryable bool) error {
	if session == nil {
		return ErrFileFrameInvalid
	}
	return forwarder.connections.EnqueueEndpoint(deviceID, epoch, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: epoch,
		Frame: &remotev1.Envelope_FileError{FileError: &remotev1.FileError{
			TransferId: session.id, Generation: session.generation, Code: code, Retryable: retryable,
		}},
	})
}

func (forwarder *FileForwarder) DisconnectDevice(_ context.Context, deviceID string) {
	forwarder.mu.Lock()
	interrupted := make([]*fileSession, 0)
	for id, session := range forwarder.sessions {
		if session.sourceDeviceID == deviceID || session.targetDeviceID == deviceID {
			copy := *session
			interrupted = append(interrupted, &copy)
			delete(forwarder.sessions, id)
		}
	}
	forwarder.mu.Unlock()
	for _, session := range interrupted {
		peerID, peerEpoch := session.sourceDeviceID, session.sourceEpoch
		if session.sourceDeviceID == deviceID {
			peerID, peerEpoch = session.targetDeviceID, session.targetEpoch
		}
		_ = forwarder.sendFileError(peerID, peerEpoch, session, remotev1.ErrorCode_ERROR_CODE_FILE_INTERRUPTED, true)
	}
}

func (forwarder *FileForwarder) deleteSession(id string) {
	forwarder.mu.Lock()
	delete(forwarder.sessions, id)
	forwarder.mu.Unlock()
}

func (forwarder *FileForwarder) sessionCountLocked(deviceID string) int {
	count := 0
	for _, session := range forwarder.sessions {
		if session.sourceDeviceID == deviceID || session.targetDeviceID == deviceID {
			count++
		}
	}
	return count
}

func (forwarder *FileForwarder) removeExpiredLocked(now time.Time) {
	for id, session := range forwarder.sessions {
		if !session.expiresAt.After(now) {
			delete(forwarder.sessions, id)
		}
	}
}
