package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/peerprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"github.com/wenzwork/wenzwork-web/server/internal/remotedevice"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type preparedPeerClient struct {
	state       clientState
	accessToken string
	allocation  allocationResponse
}

const requestedPeerSessionDurationSeconds = 30 * 24 * 60 * 60

type peerTicketResponse struct {
	SessionID             uuid.UUID `json:"sessionId"`
	PeerSessionTicket     string    `json:"peerSessionTicket"`
	ExpiresAt             time.Time `json:"expiresAt"`
	MaxDurationSeconds    uint32    `json:"maxDurationSeconds"`
	MaxBytes              uint64    `json:"maxBytes"`
	TargetKeyThumbprint   string    `json:"targetKeyThumbprint"`
	RelayURL              string    `json:"relayUrl"`
	RelayNodeID           uuid.UUID `json:"relayNodeId"`
	RelayCellID           uuid.UUID `json:"relayCellId"`
	TargetConnectionEpoch uint64    `json:"targetConnectionEpoch"`
}

// peerExchangeSamples intentionally measures only a warmed ciphertext frame:
// after the Peer session is authenticated and keys are derived, each sample
// starts at the WebSocket write and ends when the other endpoint verifies the
// authenticated ciphertext. This isolates Relay forwarding from ticket and
// TLS/WebSocket bootstrap latency.
type peerExchangeSamples struct {
	sourceToTarget []time.Duration
	targetToSource []time.Duration
}

type peerLatencySummary struct {
	Samples int64 `json:"samples"`
	P50MS   int64 `json:"p50Ms"`
	P95MS   int64 `json:"p95Ms"`
	P99MS   int64 `json:"p99Ms"`
	MaxMS   int64 `json:"maxMs"`
}

// peerColdStartTiming mirrors the three browser cold-start waits after the
// target Agent is already online: issue a ticket, authenticate the direct
// Relay WebSocket, then establish the E2EE Peer session. Values are durations
// only; no ticket, identity, endpoint, or RPC payload is reported.
type peerColdStartTiming struct {
	TicketMS   int64 `json:"ticketMs"`
	RelayMS    int64 `json:"relayMs"`
	PeerOpenMS int64 `json:"peerOpenMs"`
	TotalMS    int64 `json:"totalMs"`
}

func summarizePeerLatency(samples []time.Duration) peerLatencySummary {
	if len(samples) == 0 {
		return peerLatencySummary{}
	}
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	percentile := func(percent int) time.Duration {
		index := (len(ordered)*percent + 99) / 100
		if index < 1 {
			index = 1
		}
		return ordered[index-1]
	}
	return peerLatencySummary{
		Samples: int64(len(ordered)), P50MS: percentile(50).Milliseconds(),
		P95MS: percentile(95).Milliseconds(), P99MS: percentile(99).Milliseconds(),
		MaxMS: ordered[len(ordered)-1].Milliseconds(),
	}
}

func executePeer(ctx context.Context, opts options, report reporter) error {
	httpClient, err := secureHTTPClient(opts.tlsCAFile)
	if err != nil {
		return stageError{code: 2, err: err}
	}
	source, err := preparePeerClient(ctx, httpClient, opts.controlURL, opts.stateFile, opts.accessToken, "source", report)
	if err != nil {
		return err
	}
	target, err := preparePeerClient(ctx, httpClient, opts.controlURL, opts.targetStateFile, opts.targetAccessToken, "target", report)
	if err != nil {
		return err
	}
	if source.state.DeviceID == target.state.DeviceID {
		return stageError{code: 2, err: errors.New("Peer acceptance requires two distinct device identities")}
	}
	report.step(1, "Two devices ready", map[string]any{})
	report.step(2, "Two allocations ready", map[string]any{})

	targetRelay, err := dialPeerRelay(ctx, httpClient, target.allocation, target.state, report)
	if err != nil {
		return err
	}
	defer targetRelay.socket.CloseNow()
	report.step(3, "Target Relay ready", map[string]any{})

	coldStart := time.Now()
	ticketStarted := coldStart
	ticket, err := createPeerSessionTicket(ctx, httpClient, opts.controlURL, source.accessToken, target.state.DeviceID)
	if err != nil {
		return stageError{code: 60, err: err}
	}
	ticketElapsed := time.Since(ticketStarted)
	ticketClaims, err := parseTicketClaims(ticket.PeerSessionTicket)
	if err != nil || ticket.SessionID == uuid.Nil || ticket.SessionID.String() != ticketClaims.SessionID ||
		ticketClaims.Audience != "relay-peer" || ticketClaims.Subject != source.state.DeviceID.String() ||
		ticketClaims.SourceDeviceID != source.state.DeviceID.String() || ticketClaims.TargetDeviceID != target.state.DeviceID.String() ||
		ticketClaims.JWTID == "" || !ticket.ExpiresAt.After(time.Now()) || ticket.MaxDurationSeconds == 0 || ticket.MaxBytes == 0 ||
		ticket.RelayURL == "" || ticket.RelayNodeID == uuid.Nil || ticket.RelayCellID == uuid.Nil ||
		ticket.TargetConnectionEpoch != target.state.ConnectionEpoch || ticketClaims.RelayNodeID != ticket.RelayNodeID.String() ||
		ticketClaims.RelayCellID != ticket.RelayCellID.String() || ticketClaims.TargetConnectionEpoch != ticket.TargetConnectionEpoch {
		return stageError{code: 60, err: errors.New("Peer Session Ticket response is invalid")}
	}
	targetIdentityPublic := target.state.identity.Public().(ed25519.PublicKey)
	if ticket.TargetKeyThumbprint != remoteauth.PublicKeyThumbprint(targetIdentityPublic) {
		return stageError{code: 60, err: errors.New("Peer target identity thumbprint does not match local target state")}
	}
	if source.state.ConnectionEpoch == ^uint64(0) {
		return stageError{code: 2, err: errors.New("source direct connectionEpoch is exhausted")}
	}
	source.state.ConnectionEpoch++
	if err := writeState(opts.stateFile, source.state); err != nil {
		return stageError{code: 2, err: err}
	}
	relayStarted := time.Now()
	sourceRelay, err := dialDirectPeerRelay(ctx, httpClient, ticket, source.state)
	if err != nil {
		return err
	}
	relayElapsed := time.Since(relayStarted)
	defer sourceRelay.socket.CloseNow()

	peerOpenStarted := time.Now()
	sourceEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return stageError{code: 60, err: errors.New("generate source ephemeral key")}
	}
	openProof := remoteauth.PeerOpenIdentityProof{
		TicketJWTID: ticketClaims.JWTID, SessionID: ticketClaims.SessionID,
		SourceDeviceID: ticketClaims.SourceDeviceID, TargetDeviceID: ticketClaims.TargetDeviceID,
		EphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(),
	}
	openSignature, err := remoteauth.SignPeerOpenIdentity(source.state.identity, openProof)
	if err != nil {
		return stageError{code: 60, err: errors.New("sign PEER_OPEN")}
	}
	if err := writeRelayEnvelope(ctx, sourceRelay.socket, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: source.state.ConnectionEpoch,
		Frame: &remotev1.Envelope_PeerOpen{PeerOpen: &remotev1.PeerOpen{
			SessionTicket: ticket.PeerSessionTicket, SessionId: ticketClaims.SessionID,
			EphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(), IdentitySignature: openSignature,
		}},
	}); err != nil {
		return stageError{code: 61, err: fmt.Errorf("write PEER_OPEN: %w", err)}
	}
	targetOpenEnvelope, err := readPeerEnvelope(ctx, targetRelay.socket, target.state.ConnectionEpoch)
	if err != nil {
		return stageError{code: 61, err: fmt.Errorf("read PEER_OPEN: %w", err)}
	}
	targetOpen := targetOpenEnvelope.GetPeerOpen()
	sourceIdentityPublic := source.state.identity.Public().(ed25519.PublicKey)
	if targetOpen == nil || targetOpen.GetSessionId() != ticketClaims.SessionID || targetOpen.GetSessionTicket() != ticket.PeerSessionTicket ||
		remoteauth.VerifyPeerOpenIdentity(sourceIdentityPublic, ticketClaims.SourceKeyThumbprint, remoteauth.PeerOpenIdentityProof{
			TicketJWTID: ticketClaims.JWTID, SessionID: ticketClaims.SessionID,
			SourceDeviceID: ticketClaims.SourceDeviceID, TargetDeviceID: ticketClaims.TargetDeviceID,
			EphemeralPublicKey: targetOpen.GetEphemeralPublicKey(),
		}, targetOpen.GetIdentitySignature()) != nil {
		return stageError{code: 61, err: errors.New("PEER_OPEN identity proof is invalid")}
	}

	targetEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return stageError{code: 61, err: errors.New("generate target ephemeral key")}
	}
	readyProof := remoteauth.PeerReadyIdentityProof{
		TicketJWTID: ticketClaims.JWTID, SessionID: ticketClaims.SessionID,
		SourceDeviceID: ticketClaims.SourceDeviceID, TargetDeviceID: ticketClaims.TargetDeviceID,
		SourceEphemeralPublicKey: targetOpen.GetEphemeralPublicKey(), TargetEphemeralPublicKey: targetEphemeral.PublicKey().Bytes(),
	}
	readySignature, err := remoteauth.SignPeerReadyIdentity(target.state.identity, readyProof)
	if err != nil {
		return stageError{code: 61, err: errors.New("sign PEER_READY")}
	}
	if err := writeRelayEnvelope(ctx, targetRelay.socket, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: target.state.ConnectionEpoch,
		Frame: &remotev1.Envelope_PeerReady{PeerReady: &remotev1.PeerReady{
			SessionId: ticketClaims.SessionID, EphemeralPublicKey: targetEphemeral.PublicKey().Bytes(), IdentitySignature: readySignature,
		}},
	}); err != nil {
		return stageError{code: 61, err: fmt.Errorf("write PEER_READY: %w", err)}
	}
	sourceReadyEnvelope, err := readPeerEnvelope(ctx, sourceRelay.socket, source.state.ConnectionEpoch)
	if err != nil {
		return stageError{code: 61, err: fmt.Errorf("read PEER_READY: %w", err)}
	}
	sourceReady := sourceReadyEnvelope.GetPeerReady()
	if sourceReady == nil || remoteauth.VerifyPeerReadyIdentity(targetIdentityPublic, ticketClaims.TargetKeyThumbprint, remoteauth.PeerReadyIdentityProof{
		TicketJWTID: ticketClaims.JWTID, SessionID: ticketClaims.SessionID,
		SourceDeviceID: ticketClaims.SourceDeviceID, TargetDeviceID: ticketClaims.TargetDeviceID,
		SourceEphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(), TargetEphemeralPublicKey: sourceReady.GetEphemeralPublicKey(),
	}, sourceReady.GetIdentitySignature()) != nil {
		return stageError{code: 61, err: errors.New("PEER_READY identity proof is invalid")}
	}
	sourceSecret, err := peerprotocol.X25519SharedSecret(sourceEphemeral, sourceReady.GetEphemeralPublicKey())
	if err != nil {
		return stageError{code: 62, err: err}
	}
	targetSecret, err := peerprotocol.X25519SharedSecret(targetEphemeral, targetOpen.GetEphemeralPublicKey())
	if err != nil {
		return stageError{code: 62, err: err}
	}
	sourceKeys, err := peerprotocol.DeriveSessionKeys(sourceSecret, ticketClaims.JWTID, ticketClaims.SessionID, ticketClaims.SourceDeviceID, ticketClaims.TargetDeviceID)
	if err != nil {
		return stageError{code: 62, err: err}
	}
	targetKeys, err := peerprotocol.DeriveSessionKeys(targetSecret, ticketClaims.JWTID, ticketClaims.SessionID, ticketClaims.SourceDeviceID, ticketClaims.TargetDeviceID)
	if err != nil || !bytes.Equal(sourceKeys.SourceToTarget, targetKeys.SourceToTarget) || !bytes.Equal(sourceKeys.TargetToSource, targetKeys.TargetToSource) {
		return stageError{code: 62, err: errors.New("Peer end-to-end session keys do not match")}
	}
	coldTiming := peerColdStartTiming{
		TicketMS: ticketElapsed.Milliseconds(), RelayMS: relayElapsed.Milliseconds(),
		PeerOpenMS: time.Since(peerOpenStarted).Milliseconds(), TotalMS: time.Since(coldStart).Milliseconds(),
	}
	report.step(4, "Peer Session active", map[string]any{
		"ticketMs": coldTiming.TicketMS, "relayMs": coldTiming.RelayMS,
		"peerOpenMs": coldTiming.PeerOpenMS, "coldStartMs": coldTiming.TotalMS,
	})

	samples := &peerExchangeSamples{}
	if err := exchangePeerMessages(ctx, sourceRelay.socket, targetRelay.socket, source.state.ConnectionEpoch, target.state.ConnectionEpoch, ticketClaims.SessionID, sourceKeys, targetKeys, opts.messageCount, samples); err != nil {
		return stageError{code: 62, err: err}
	}
	sourceLatency := summarizePeerLatency(samples.sourceToTarget)
	targetLatency := summarizePeerLatency(samples.targetToSource)
	report.step(5, fmt.Sprintf("Encrypted %d×2", opts.messageCount), map[string]any{
		"sourceToTargetP95Ms": sourceLatency.P95MS, "targetToSourceP95Ms": targetLatency.P95MS,
	})
	if opts.jsonOutput {
		_ = jsonResult(report, opts.messageCount, coldTiming, sourceLatency, targetLatency)
	} else {
		fmt.Fprintln(report.stdout, "RESULT: PASS")
	}
	_ = sourceRelay.socket.Close(websocket.StatusNormalClosure, "Peer acceptance complete")
	_ = targetRelay.socket.Close(websocket.StatusNormalClosure, "Peer acceptance complete")
	return nil
}

func preparePeerClient(ctx context.Context, httpClient *http.Client, controlURL *url.URL, stateFile, suppliedAccessToken, label string, report reporter) (preparedPeerClient, error) {
	state, err := loadOrCreateState(stateFile)
	if err != nil {
		return preparedPeerClient{}, stageError{code: 2, err: err}
	}
	deviceName, _ := os.Hostname()
	deviceName = strings.TrimSpace(deviceName) + " peer-" + label
	accessToken := suppliedAccessToken
	if accessToken == "" && state.RefreshToken != "" && state.SessionID != uuid.Nil {
		refreshed, refreshErr := refreshAccessToken(ctx, httpClient, controlURL, state.RefreshToken)
		if refreshErr == nil && slices.Contains(strings.Fields(refreshed.Scope), "remote.connect") {
			accessToken, state.RefreshToken, state.SessionID = refreshed.AccessToken, refreshed.RefreshToken, refreshed.SessionID
		}
	}
	if accessToken == "" {
		tokens, oauthErr := deviceOAuth(ctx, httpClient, controlURL, state.DeviceID, deviceName, report)
		if oauthErr != nil {
			return preparedPeerClient{}, stageError{code: 10, err: oauthErr}
		}
		accessToken, state.RefreshToken, state.SessionID = tokens.AccessToken, tokens.RefreshToken, tokens.SessionID
	}
	if state.SessionID == uuid.Nil {
		return preparedPeerClient{}, stageError{code: 10, err: errors.New("supplied access tokens require state files containing sessionId")}
	}
	publicKey := state.identity.Public().(ed25519.PublicKey)
	proof, err := remotedevice.SignRegistration(state.identity, state.SessionID, state.DeviceID)
	if err != nil {
		return preparedPeerClient{}, stageError{code: 20, err: err}
	}
	if _, err := registerDevice(ctx, httpClient, controlURL, accessToken, state, deviceName, publicKey, proof); err != nil {
		return preparedPeerClient{}, stageError{code: 20, err: err}
	}
	if state.ConnectionEpoch == ^uint64(0) {
		return preparedPeerClient{}, stageError{code: 2, err: errors.New("connectionEpoch is exhausted")}
	}
	state.ConnectionEpoch++
	if err := writeState(stateFile, state); err != nil {
		return preparedPeerClient{}, stageError{code: 2, err: err}
	}
	allocation, err := createAllocation(ctx, httpClient, controlURL, accessToken, state)
	if err != nil {
		return preparedPeerClient{}, stageError{code: 30, err: err}
	}
	return preparedPeerClient{state: state, accessToken: accessToken, allocation: allocation}, nil
}

func dialPeerRelay(ctx context.Context, client *http.Client, allocation allocationResponse, state clientState, report reporter) (authenticatedRelayConnection, error) {
	endpoints := append([]allocationEndpoint{allocation.Primary}, allocation.Fallbacks...)
	var lastErr error
	for _, endpoint := range endpoints {
		connected, err := dialRelayEndpoint(ctx, client, endpoint, allocation.AssignmentID, allocation.ConnectionTicket, state)
		if err == nil {
			return connected, nil
		}
		lastErr = err
		report.debug(fmt.Sprintf("Relay Cell %s Peer attempt failed: %v", endpoint.CellID, err))
	}
	if lastErr == nil {
		lastErr = errors.New("allocation did not contain a Relay endpoint")
	}
	return authenticatedRelayConnection{}, lastErr
}

func createPeerSessionTicket(ctx context.Context, client *http.Client, base *url.URL, accessToken string, targetDeviceID uuid.UUID) (peerTicketResponse, error) {
	var response peerTicketResponse
	err := doJSON(ctx, client, http.MethodPost, endpointURL(base, "/v1/device/peer-session-tickets"), accessToken, "peer-ticket-"+uuid.NewString(), map[string]any{
		"targetDeviceId": targetDeviceID, "scope": "remote.peer.query", "requestedMaxDurationSeconds": requestedPeerSessionDurationSeconds, "requestedMaxBytes": 16 << 20,
	}, &response)
	return response, err
}

func exchangePeerMessages(ctx context.Context, source, target *websocket.Conn, sourceEpoch, targetEpoch uint64, sessionID string, sourceKeys, targetKeys peerprotocol.SessionKeys, count int, samples *peerExchangeSamples) error {
	const queryID = "relay-client-acceptance"
	pacer := time.NewTicker(15 * time.Millisecond)
	defer pacer.Stop()
	deadline := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Millisecond)
	for index := 1; index <= count; index++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pacer.C:
		}
		sequence := uint64(index)
		metadata := peerprotocol.CiphertextMetadata{
			FrameType: "PEER_QUERY", SessionID: sessionID, QueryID: queryID, Generation: 1,
			MessageSequence: sequence, Deadline: deadline, Direction: peerprotocol.DirectionSourceToTarget,
		}
		plaintext := []byte(fmt.Sprintf("source-message-%06d", index))
		ciphertext, err := peerprotocol.Seal(sourceKeys.SourceToTarget, plaintext, metadata)
		if err != nil {
			return err
		}
		started := time.Now()
		if err := writeRelayEnvelope(ctx, source, peerCiphertextEnvelope(sourceEpoch, metadata, ciphertext)); err != nil {
			return fmt.Errorf("write source Peer message %d: %w", index, err)
		}
		received, err := readPeerEnvelope(ctx, target, targetEpoch)
		if err != nil {
			return fmt.Errorf("read target Peer message %d: %w", index, err)
		}
		frame := received.GetPeerQuery()
		if frame == nil || frame.GetMessageSequence() != sequence {
			return fmt.Errorf("target Peer sequence %d is invalid", index)
		}
		opened, err := peerprotocol.Open(targetKeys.SourceToTarget, frame.GetCiphertext(), metadata)
		if err != nil || !bytes.Equal(opened, plaintext) {
			return fmt.Errorf("target Peer ciphertext %d failed authentication", index)
		}
		if samples != nil {
			samples.sourceToTarget = append(samples.sourceToTarget, time.Since(started))
		}
	}
	for index := 1; index <= count; index++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pacer.C:
		}
		sequence := uint64(index)
		metadata := peerprotocol.CiphertextMetadata{
			FrameType: "PEER_DELTA", SessionID: sessionID, QueryID: queryID, Generation: 1,
			MessageSequence: sequence, Direction: peerprotocol.DirectionTargetToSource,
		}
		plaintext := []byte(fmt.Sprintf("target-message-%06d", index))
		ciphertext, err := peerprotocol.Seal(targetKeys.TargetToSource, plaintext, metadata)
		if err != nil {
			return err
		}
		started := time.Now()
		if err := writeRelayEnvelope(ctx, target, peerCiphertextEnvelope(targetEpoch, metadata, ciphertext)); err != nil {
			return fmt.Errorf("write target Peer message %d: %w", index, err)
		}
		received, err := readPeerEnvelope(ctx, source, sourceEpoch)
		if err != nil {
			return fmt.Errorf("read source Peer message %d: %w", index, err)
		}
		frame := received.GetPeerDelta()
		if frame == nil || frame.GetMessageSequence() != sequence {
			return fmt.Errorf("source Peer sequence %d is invalid", index)
		}
		opened, err := peerprotocol.Open(sourceKeys.TargetToSource, frame.GetCiphertext(), metadata)
		if err != nil || !bytes.Equal(opened, plaintext) {
			return fmt.Errorf("source Peer ciphertext %d failed authentication", index)
		}
		if samples != nil {
			samples.targetToSource = append(samples.targetToSource, time.Since(started))
		}
	}
	return nil
}

func peerCiphertextEnvelope(epoch uint64, metadata peerprotocol.CiphertextMetadata, ciphertext []byte) *remotev1.Envelope {
	frame := &remotev1.PeerCiphertext{
		SessionId: metadata.SessionID, QueryId: metadata.QueryID, Generation: metadata.Generation,
		MessageSequence: metadata.MessageSequence, Ciphertext: ciphertext,
	}
	if !metadata.Deadline.IsZero() {
		frame.Deadline = timestamppb.New(metadata.Deadline)
	}
	envelope := &remotev1.Envelope{ProtocolVersion: 1, ConnectionEpoch: epoch}
	if metadata.FrameType == "PEER_QUERY" {
		envelope.Frame = &remotev1.Envelope_PeerQuery{PeerQuery: frame}
	} else {
		envelope.Frame = &remotev1.Envelope_PeerDelta{PeerDelta: frame}
	}
	return envelope
}

func readPeerEnvelope(ctx context.Context, connection *websocket.Conn, epoch uint64) (*remotev1.Envelope, error) {
	envelope, err := readRelayEnvelope(ctx, connection)
	if err != nil {
		return nil, err
	}
	if goAway := envelope.GetGoAway(); goAway != nil {
		return nil, goAwayMessage(goAway)
	}
	if peerError := envelope.GetPeerError(); peerError != nil {
		return nil, fmt.Errorf("Peer error code=%s retryable=%t", peerError.GetCode(), peerError.GetRetryable())
	}
	if envelope.GetProtocolVersion() != 1 || envelope.GetConnectionEpoch() != epoch {
		return nil, errors.New("Peer frame connection epoch is invalid")
	}
	return envelope, nil
}

func jsonResult(report reporter, count int, coldStart peerColdStartTiming, sourceToTarget, targetToSource peerLatencySummary) error {
	return json.NewEncoder(report.stdout).Encode(map[string]any{
		"event": "result", "result": "PASS", "peerMessagesEachDirection": count,
		"coldStart": coldStart, "sourceToTarget": sourceToTarget, "targetToSource": targetToSource,
	})
}
