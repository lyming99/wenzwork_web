package remotepoc

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/wenzwork/wenzwork-web/server/internal/commands"
	"github.com/wenzwork/wenzwork-web/server/internal/fileprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/relayallocation"
	"github.com/wenzwork/wenzwork-web/server/internal/relayprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

type Outcome struct {
	InitialCell             string `json:"initialCell"`
	InitialNode             string `json:"initialNode"`
	MigratedCell            string `json:"migratedCell"`
	MigratedNode            string `json:"migratedNode"`
	AssignmentVersionBefore uint64 `json:"assignmentVersionBefore"`
	AssignmentVersionAfter  uint64 `json:"assignmentVersionAfter"`
	ConnectionEpochBefore   uint64 `json:"connectionEpochBefore"`
	ConnectionEpochAfter    uint64 `json:"connectionEpochAfter"`
	OldRouteFenced          bool   `json:"oldRouteFenced"`
	GoAwayHandled           bool   `json:"goAwayHandled"`
	CommandDeliveries       int    `json:"commandDeliveries"`
	CommandSideEffects      int64  `json:"commandSideEffects"`
	FileRoundTripBytes      int    `json:"fileRoundTripBytes"`
}

func Run(ctx context.Context, now time.Time) (Outcome, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cells := []relayallocation.Cell{
		pocCell("r017", 10, "r017-node-0", "r017-node-1"),
		pocCell("r018", 200, "r018-node-0", "r018-node-1"),
	}
	scheduler := relayallocation.Scheduler{}
	assignmentSequence := 0
	assignmentID := func() string {
		assignmentSequence++
		return fmt.Sprintf("assignment-%d", assignmentSequence)
	}
	initial, err := scheduler.Allocate(relayallocation.Request{
		UserID: "poc-user", Region: "cn-hangzhou", Pool: "standard", ProtocolVersion: 1,
		Now: now, AssignmentID: assignmentID,
	}, cells)
	if err != nil {
		return Outcome{}, fmt.Errorf("initial allocation: %w", err)
	}
	initialNode := nodeForCell(cells, initial.CellID)
	agent := relayprotocol.NewAgentRelayState(0)
	initialEpoch, err := agent.BeginConnect()
	if err != nil {
		return Outcome{}, err
	}

	signerPublic, signerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Outcome{}, err
	}
	devicePublic, devicePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Outcome{}, err
	}
	issuer := remoteauth.Issuer{Issuer: "wenzwork-control-poc", KeyID: "poc-ticket-key", PrivateKey: signerPrivate}
	verifier := remoteauth.Verifier{Issuer: "wenzwork-control-poc", Keys: map[string]ed25519.PublicKey{"poc-ticket-key": signerPublic}}
	claims := connectionClaims(now, initial, devicePublic)
	token, err := issuer.Sign(claims)
	if err != nil {
		return Outcome{}, err
	}
	verified, err := verifier.Verify(token, "relay", now)
	if err != nil {
		return Outcome{}, err
	}
	if err := verified.ValidateConnection("poc-device", "poc-user", initial.CellID, remoteauth.PublicKeyThumbprint(devicePublic), initial.Version, 1, 1); err != nil {
		return Outcome{}, err
	}
	challenge, err := remoteauth.NewChallenge(verified.JWTID, initial.CellID, initialNode, initialEpoch, now, 30*time.Second)
	if err != nil {
		return Outcome{}, err
	}
	proof, err := remoteauth.SignChallenge(devicePrivate, challenge)
	if err != nil {
		return Outcome{}, err
	}
	if err := remoteauth.VerifyChallenge(devicePublic, verified.Confirmation, challenge, proof, now); err != nil {
		return Outcome{}, err
	}

	registry := relayrouter.NewRegistry()
	registry.PutAssignmentFence("poc-user", relayrouter.AssignmentFence{Version: initial.Version, AllowedCellIDs: append([]string{initial.CellID}, initial.FallbackCellIDs...)})
	registry.PutGrantFence("poc-device", relayrouter.GrantFence{Version: 1, Status: relayrouter.DeviceActive})
	if err := registry.Register(relayrouter.Route{
		DeviceID: "poc-device", UserID: "poc-user", CellID: initial.CellID, NodeID: initialNode,
		ConnectionID: "connection-1", ConnectionEpoch: initialEpoch, AssignmentVersion: initial.Version,
		GrantVersion: 1, ProtocolVersion: 1,
	}, 2*time.Minute, now); err != nil {
		return Outcome{}, err
	}
	if err := agent.MarkReady(initialEpoch); err != nil {
		return Outcome{}, err
	}

	inbox := commands.NewInbox()
	var sideEffects atomic.Int64
	const deliveries = 100
	for range deliveries {
		result, _, err := inbox.Execute(ctx, "command-1", now.Add(10*time.Minute), now, func(context.Context) ([]byte, error) {
			sideEffects.Add(1)
			return []byte("task-created"), nil
		})
		if err != nil || string(result.Payload) != "task-created" {
			return Outcome{}, fmt.Errorf("command delivery: result=%q error=%w", result.Payload, err)
		}
	}

	for index := range cells {
		if cells[index].ID == initial.CellID {
			cells[index].Status = relayallocation.CellDraining
		}
	}
	reconnect, err := agent.HandleGoAway(time.Second, true)
	if err != nil {
		return Outcome{}, err
	}
	migrated, err := scheduler.Allocate(relayallocation.Request{
		UserID: "poc-user", Region: "cn-hangzhou", Pool: "standard", ProtocolVersion: 1,
		Current: &initial, Now: now.Add(time.Minute), AssignmentID: assignmentID,
	}, cells)
	if err != nil {
		return Outcome{}, fmt.Errorf("migration allocation: %w", err)
	}
	migratedNode := nodeForCell(cells, migrated.CellID)
	migratedNow := now.Add(time.Minute)
	migratedEpoch, err := agent.BeginConnect()
	if err != nil {
		return Outcome{}, err
	}
	migratedToken, err := issuer.Sign(connectionClaims(migratedNow, migrated, devicePublic))
	if err != nil {
		return Outcome{}, err
	}
	migratedClaims, err := verifier.Verify(migratedToken, "relay", migratedNow)
	if err != nil {
		return Outcome{}, err
	}
	if err := migratedClaims.ValidateConnection("poc-device", "poc-user", migrated.CellID, remoteauth.PublicKeyThumbprint(devicePublic), migrated.Version, 1, 1); err != nil {
		return Outcome{}, err
	}
	migratedChallenge, err := remoteauth.NewChallenge(migratedClaims.JWTID, migrated.CellID, migratedNode, migratedEpoch, migratedNow, 30*time.Second)
	if err != nil {
		return Outcome{}, err
	}
	migratedProof, err := remoteauth.SignChallenge(devicePrivate, migratedChallenge)
	if err != nil {
		return Outcome{}, err
	}
	if err := remoteauth.VerifyChallenge(devicePublic, migratedClaims.Confirmation, migratedChallenge, migratedProof, migratedNow); err != nil {
		return Outcome{}, err
	}
	registry.PutAssignmentFence("poc-user", relayrouter.AssignmentFence{Version: migrated.Version, AllowedCellIDs: append([]string{migrated.CellID}, migrated.FallbackCellIDs...)})
	_, oldRouteErr := registry.Resolve("poc-device", migratedNow)
	oldRouteFenced := errors.Is(oldRouteErr, relayrouter.ErrAssignmentStale)
	if !oldRouteFenced {
		return Outcome{}, fmt.Errorf("old route was not fenced: %w", oldRouteErr)
	}
	if err := registry.Register(relayrouter.Route{
		DeviceID: "poc-device", UserID: "poc-user", CellID: migrated.CellID, NodeID: migratedNode,
		ConnectionID: "connection-2", ConnectionEpoch: migratedEpoch, AssignmentVersion: migrated.Version,
		GrantVersion: 1, ProtocolVersion: 1,
	}, time.Minute, migratedNow); err != nil {
		return Outcome{}, fmt.Errorf("register migrated route: %w", err)
	}
	if err := agent.MarkReady(migratedEpoch); err != nil {
		return Outcome{}, err
	}

	fileBytes, err := fileRoundTrip()
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{
		InitialCell: initial.CellID, InitialNode: initialNode, MigratedCell: migrated.CellID, MigratedNode: migratedNode,
		AssignmentVersionBefore: initial.Version, AssignmentVersionAfter: migrated.Version,
		ConnectionEpochBefore: initialEpoch, ConnectionEpochAfter: migratedEpoch, OldRouteFenced: oldRouteFenced,
		GoAwayHandled:     reconnect.RefreshAssignment && agent.State == relayprotocol.AgentReady,
		CommandDeliveries: deliveries, CommandSideEffects: sideEffects.Load(), FileRoundTripBytes: fileBytes,
	}, nil
}

func pocCell(id string, connections int64, nodeIDs ...string) relayallocation.Cell {
	nodes := make([]relayallocation.Node, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		nodes = append(nodes, relayallocation.Node{ID: nodeID, Healthy: true})
	}
	return relayallocation.Cell{
		ID: id, Region: "cn-hangzhou", Pool: "standard", Status: relayallocation.CellActive,
		Endpoint: "wss://" + id + ".relay.example/v1/connect", EndpointRevision: 1, EndpointActive: true,
		ProtocolMin: 1, ProtocolMax: 1, Weight: 1, ActiveConnections: connections,
		ConnectionSoftLimit: 1000, ConnectionHardLimit: 1200, EgressSoftLimitMbps: 1000,
		MemorySoftLimitBytes: 1 << 30, WriteLoopLagLimit: 100, Nodes: nodes,
	}
}

func nodeForCell(cells []relayallocation.Cell, cellID string) string {
	for _, cell := range cells {
		if cell.ID == cellID && len(cell.Nodes) > 0 {
			return cell.Nodes[0].ID
		}
	}
	return ""
}

func connectionClaims(now time.Time, assignment relayallocation.Assignment, publicKey ed25519.PublicKey) remoteauth.Claims {
	return remoteauth.Claims{
		Audience: "relay", Subject: "poc-device", UserID: "poc-user", AssignmentID: assignment.ID,
		AssignmentVersion: assignment.Version, AllowedCellIDs: append([]string{assignment.CellID}, assignment.FallbackCellIDs...),
		GrantVersion: 1, Scopes: []string{"remote.connect", "remote.device.read", "remote.task.read", "remote.task.write"},
		ProtocolMin: 1, ProtocolMax: 1, Confirmation: remoteauth.PublicKeyThumbprint(publicKey),
		IdentityKey: base64.RawURLEncoding.EncodeToString(publicKey),
		JWTID:       fmt.Sprintf("ticket-v%d", assignment.Version), IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}
}

func fileRoundTrip() (int, error) {
	sourceIdentityPublic, sourceIdentityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return 0, err
	}
	targetIdentityPublic, targetIdentityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return 0, err
	}
	sourceEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return 0, err
	}
	targetEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return 0, err
	}
	plaintext := []byte("cross-node E2EE POC")
	open := fileprotocol.OpenTranscript{
		TicketJWTID: "file-ticket-1", TransferID: "transfer-1", Generation: 1,
		SourceDeviceID: "device-a", TargetDeviceID: "device-b", SourceEphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(),
		DeclaredTotalBytes: uint64(len(plaintext)), DeclaredFileCount: 1,
	}
	openSignature, err := fileprotocol.SignOpen(sourceIdentityPrivate, open)
	if err != nil {
		return 0, err
	}
	if err := fileprotocol.VerifyOpen(sourceIdentityPublic, open, openSignature); err != nil {
		return 0, err
	}
	accept := fileprotocol.AcceptTranscript{
		TargetEphemeralPublicKey: targetEphemeral.PublicKey().Bytes(), CipherSuite: fileprotocol.XChaCha20Poly1305,
		ChunkSize: fileprotocol.DefaultChunkSize, ReceiveWindow: 1 << 20,
	}
	acceptSignature, err := fileprotocol.SignAccept(targetIdentityPrivate, open, openSignature, accept)
	if err != nil {
		return 0, err
	}
	if err := fileprotocol.VerifyAccept(targetIdentityPublic, open, openSignature, accept, acceptSignature); err != nil {
		return 0, err
	}
	transcriptHash, err := (fileprotocol.Handshake{Open: open, OpenSignature: openSignature, Accept: accept, AcceptSignature: acceptSignature}).TranscriptHash()
	if err != nil {
		return 0, err
	}
	sharedSecret, err := fileprotocol.X25519SharedSecret(sourceEphemeral, accept.TargetEphemeralPublicKey)
	if err != nil {
		return 0, err
	}
	keys, err := fileprotocol.DeriveSessionKeys(sharedSecret, open.TicketJWTID, open.TransferID, open.Generation, transcriptHash)
	if err != nil {
		return 0, err
	}
	fileID := []byte("poc-file-id-0001")
	fileKey, err := fileprotocol.DeriveFileKey(keys.FileMasterKey, fileID)
	if err != nil {
		return 0, err
	}
	manifestHash := sha256.Sum256([]byte("poc-manifest"))
	metadata := fileprotocol.ChunkMetadata{
		ProtocolVersion: 1, TransferID: open.TransferID, Generation: open.Generation, FileID: fileID,
		ChunkIndex: 0, Offset: 0, PlaintextSize: uint32(len(plaintext)), TotalFileSize: uint64(len(plaintext)),
		Direction: fileprotocol.DirectionSenderToReceiver, ManifestHash: manifestHash[:],
	}
	noncePrefix := []byte("poc-nonce-prefix")
	ciphertext, err := fileprotocol.SealChunk(fileKey, noncePrefix, plaintext, metadata)
	if err != nil {
		return 0, err
	}
	opened, err := fileprotocol.OpenChunk(fileKey, noncePrefix, ciphertext, metadata)
	if err != nil || string(opened) != string(plaintext) {
		return 0, fmt.Errorf("file E2EE round trip failed: %w", err)
	}
	return len(opened), nil
}
