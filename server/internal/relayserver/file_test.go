package relayserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/fileprotocol"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

type fileRegistryStub struct{ consumed map[string]bool }

func (registry *fileRegistryStub) ConsumeFileTicket(_ context.Context, jwtID string, _, _ time.Time) error {
	if registry.consumed == nil {
		registry.consumed = make(map[string]bool)
	}
	if registry.consumed[jwtID] {
		return ErrFileFrameInvalid
	}
	registry.consumed[jwtID] = true
	return nil
}

type fileFixture struct {
	now                                      time.Time
	sourceID, targetID, transferID, ticketID string
	sourceEpoch, targetEpoch                 uint64
	sourcePrivate, targetPrivate             ed25519.PrivateKey
	ticketPrivate                            ed25519.PrivateKey
	forwarder                                *FileForwarder
	sourceFrames, targetFrames               chan *remotev1.Envelope
	ticket                                   string
	openTranscript                           fileprotocol.OpenTranscript
}

func newFileFixture(t *testing.T, targetOnline bool) fileFixture {
	t.Helper()
	now := time.Date(2026, 8, 8, 8, 0, 0, 0, time.UTC)
	sourceID, targetID, transferID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	sourcePublic, sourcePrivate, _ := ed25519.GenerateKey(rand.Reader)
	targetPublic, targetPrivate, _ := ed25519.GenerateKey(rand.Reader)
	ticketPublic, ticketPrivate, _ := ed25519.GenerateKey(rand.Reader)
	ticketID := uuid.NewString()
	claims := remoteauth.Claims{
		Audience: "relay-file", Subject: sourceID, UserID: uuid.NewString(), TransferID: transferID,
		SourceDeviceID: sourceID, TargetDeviceID: targetID, Direction: "push",
		SourceGrantVersion: 2, TargetGrantVersion: 3,
		SourceKeyThumbprint: remoteauth.PublicKeyThumbprint(sourcePublic), TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic),
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourcePublic), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetPublic),
		SourceKeyVersion: 4, TargetKeyVersion: 5, SourceCredentialType: "device", Confirmation: remoteauth.PublicKeyThumbprint(sourcePublic),
		RelayNodeID: "node-file", RelayCellID: "cell-file", TargetConnectionEpoch: 22,
		Scopes: []string{"remote.peer.file.send", "remote.peer.file.receive"}, MaxDurationSeconds: 600,
		MaxBytes: 4096, MaxFileCount: 2, MaxManifestBytes: 1024, AllowedChunkSize: 64 << 10, RequireLocalApproval: true,
		JWTID: ticketID, IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	ticket, err := (remoteauth.Issuer{Issuer: "control", KeyID: "file-key", PrivateKey: ticketPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := NewConnectionManager(8, 2)
	sourceConnection := &peerFrameConnection{frames: make(chan *remotev1.Envelope, 32)}
	targetConnection := &peerFrameConnection{frames: make(chan *remotev1.Envelope, 32)}
	if _, err := manager.Attach(sourceID, "source", 11, sourceConnection); err != nil {
		t.Fatal(err)
	}
	if targetOnline {
		if _, err := manager.Attach(targetID, "target", 22, targetConnection); err != nil {
			t.Fatal(err)
		}
	}
	forwarder, err := NewFileForwarder(FileForwarderConfig{
		NodeID: "node-file", CellID: "cell-file",
		Verifier: remoteauth.Verifier{Issuer: "control", Keys: map[string]ed25519.PublicKey{"file-key": ticketPublic}},
		Devices:  peerDeviceResolverStub{keys: map[string]ed25519.PublicKey{sourceID: sourcePublic, targetID: targetPublic}},
		Routes:   &fileRegistryStub{}, Connections: manager, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.Detach(sourceID, "source", 11)
		if targetOnline {
			manager.Detach(targetID, "target", 22)
		}
	})
	return fileFixture{
		now: now, sourceID: sourceID, targetID: targetID, transferID: transferID, ticketID: ticketID,
		sourceEpoch: 11, targetEpoch: 22, sourcePrivate: sourcePrivate, targetPrivate: targetPrivate,
		ticketPrivate: ticketPrivate, forwarder: forwarder, sourceFrames: sourceConnection.frames, targetFrames: targetConnection.frames,
		ticket: ticket,
		openTranscript: fileprotocol.OpenTranscript{
			TicketJWTID: ticketID, TransferID: transferID, Generation: 1, SourceDeviceID: sourceID, TargetDeviceID: targetID,
			SourceEphemeralPublicKey: bytesOf(32, 7), DeclaredTotalBytes: 2048, DeclaredFileCount: 2,
		},
	}
}

func (fixture fileFixture) openEnvelope(t *testing.T) *remotev1.Envelope {
	t.Helper()
	signature, err := fileprotocol.SignOpen(fixture.sourcePrivate, fixture.openTranscript)
	if err != nil {
		t.Fatal(err)
	}
	return &remotev1.Envelope{ProtocolVersion: 1, ConnectionEpoch: fixture.sourceEpoch,
		Frame: &remotev1.Envelope_FileOpen{FileOpen: &remotev1.FileOpen{
			FileTicket: fixture.ticket, TransferId: fixture.transferID, Generation: 1,
			EphemeralPublicKey: fixture.openTranscript.SourceEphemeralPublicKey, IdentitySignature: signature,
			DeclaredTotalBytes: fixture.openTranscript.DeclaredTotalBytes, DeclaredFileCount: fixture.openTranscript.DeclaredFileCount,
		}}}
}

func (fixture fileFixture) acceptEnvelope(t *testing.T) *remotev1.Envelope {
	t.Helper()
	openSignature, _ := fileprotocol.SignOpen(fixture.sourcePrivate, fixture.openTranscript)
	accept := fileprotocol.AcceptTranscript{
		TargetEphemeralPublicKey: bytesOf(32, 77), CipherSuite: fileprotocol.XChaCha20Poly1305,
		ChunkSize: 64 << 10, ReceiveWindow: 1 << 20,
	}
	signature, err := fileprotocol.SignAccept(fixture.targetPrivate, fixture.openTranscript, openSignature, accept)
	if err != nil {
		t.Fatal(err)
	}
	return &remotev1.Envelope{ProtocolVersion: 1, ConnectionEpoch: fixture.targetEpoch,
		Frame: &remotev1.Envelope_FileAccept{FileAccept: &remotev1.FileAccept{
			TransferId: fixture.transferID, Generation: 1, EphemeralPublicKey: accept.TargetEphemeralPublicKey,
			IdentitySignature: signature, CipherSuite: remotev1.FileCipherSuite_FILE_CIPHER_SUITE_XCHACHA20_POLY1305,
			ChunkSizeBytes: accept.ChunkSize, ReceiveWindowBytes: accept.ReceiveWindow,
		}}}
}

func TestFileForwarderRequiresSignedAcceptanceThenRoutesOrderedCiphertext(t *testing.T) {
	fixture := newFileFixture(t, true)
	if err := fixture.forwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, fixture.openEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	if got := receivePeerFrame(t, fixture.targetFrames); got.GetFileOpen() == nil || got.GetConnectionEpoch() != fixture.targetEpoch {
		t.Fatalf("target FILE_OPEN = %+v", got)
	}
	premature := fileCiphertextEnvelope(fixture.transferID, fixture.sourceEpoch, 1, "manifest")
	if err := fixture.forwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, premature); !errors.Is(err, ErrFileFrameInvalid) {
		t.Fatalf("premature payload error = %v", err)
	}
	if err := fixture.forwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, fixture.acceptEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	if got := receivePeerFrame(t, fixture.sourceFrames); got.GetFileAccept() == nil || got.GetConnectionEpoch() != fixture.sourceEpoch {
		t.Fatalf("source FILE_ACCEPT = %+v", got)
	}
	manifest := fileCiphertextEnvelope(fixture.transferID, fixture.sourceEpoch, 1, "manifest")
	if err := fixture.forwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, manifest); err != nil {
		t.Fatal(err)
	}
	if got := receivePeerFrame(t, fixture.targetFrames).GetFileManifest(); got == nil || got.GetMessageSequence() != 1 {
		t.Fatalf("manifest = %+v", got)
	}
	ack := fileCiphertextEnvelope(fixture.transferID, fixture.targetEpoch, 1, "ack")
	if err := fixture.forwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, ack); err != nil {
		t.Fatal(err)
	}
	if got := receivePeerFrame(t, fixture.sourceFrames).GetFileAck(); got == nil || got.GetMessageSequence() != 1 {
		t.Fatalf("ack = %+v", got)
	}
	if err := fixture.forwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, ack); !errors.Is(err, ErrFileFrameInvalid) {
		t.Fatalf("replayed ACK error = %v", err)
	}
	stale := fileCiphertextEnvelope(fixture.transferID, fixture.sourceEpoch, 2, "chunk")
	stale.GetFileChunk().Generation = 2
	if err := fixture.forwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, stale); !errors.Is(err, ErrFileFrameInvalid) {
		t.Fatalf("stale generation error = %v", err)
	}
}

func TestFileForwarderRejectsTamperedProofAndOfflineTargetWithoutClosingDeviceRoute(t *testing.T) {
	tampered := newFileFixture(t, true)
	open := tampered.openEnvelope(t)
	open.GetFileOpen().IdentitySignature[0] ^= 0x80
	if err := tampered.forwarder.HandleFromDevice(context.Background(), tampered.sourceID, tampered.sourceEpoch, open); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, tampered.sourceFrames).GetFileReject().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_FILE_TICKET_INVALID {
		t.Fatalf("tampered reject code = %s", code)
	}
	offline := newFileFixture(t, false)
	if err := offline.forwarder.HandleFromDevice(context.Background(), offline.sourceID, offline.sourceEpoch, offline.openEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, offline.sourceFrames).GetFileReject().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_FILE_TARGET_OFFLINE {
		t.Fatalf("offline reject code = %s", code)
	}
}

func fileCiphertextEnvelope(transferID string, epoch, sequence uint64, kind string) *remotev1.Envelope {
	ciphertext := &remotev1.FileCiphertext{TransferId: transferID, Generation: 1, MessageSequence: sequence, Ciphertext: []byte("encrypted-only")}
	envelope := &remotev1.Envelope{ProtocolVersion: 1, ConnectionEpoch: epoch}
	switch kind {
	case "manifest":
		envelope.Frame = &remotev1.Envelope_FileManifest{FileManifest: ciphertext}
	case "chunk":
		envelope.Frame = &remotev1.Envelope_FileChunk{FileChunk: ciphertext}
	case "ack":
		envelope.Frame = &remotev1.Envelope_FileAck{FileAck: ciphertext}
	}
	return envelope
}
