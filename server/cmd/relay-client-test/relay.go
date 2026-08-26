package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayserver"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

type relayResult struct {
	HeartbeatSeconds uint32
	RTTs             []time.Duration
	Min              time.Duration
	Average          time.Duration
	Max              time.Duration
}

type authenticatedRelayConnection struct {
	socket           *websocket.Conn
	heartbeatSeconds uint32
}

func connectRelay(ctx context.Context, client *http.Client, allocation allocationResponse, state clientState, pingCount int, report reporter) (relayResult, error) {
	endpoints := append([]allocationEndpoint{allocation.Primary}, allocation.Fallbacks...)
	var lastErr error
	for index, endpoint := range endpoints {
		if index > 0 {
			report.debug(fmt.Sprintf("Primary failed; trying fallback Cell %s", endpoint.CellID))
		}
		result, err := connectEndpoint(ctx, client, endpoint, allocation.AssignmentID, allocation.ConnectionTicket, state, pingCount, report)
		if err == nil {
			return result, nil
		}
		lastErr = err
		report.debug(fmt.Sprintf("Relay Cell %s attempt failed: %v", endpoint.CellID, err))
	}
	if lastErr == nil {
		lastErr = stageError{code: 40, err: errors.New("allocation did not contain a Relay endpoint")}
	}
	return relayResult{}, lastErr
}

func connectEndpoint(ctx context.Context, client *http.Client, endpoint allocationEndpoint, assignmentID uuid.UUID, ticket string, state clientState, pingCount int, report reporter) (relayResult, error) {
	authenticated, err := dialRelayEndpoint(ctx, client, endpoint, assignmentID, ticket, state)
	if err != nil {
		return relayResult{}, err
	}
	connection := authenticated.socket
	defer connection.CloseNow()
	report.step(4, "Relay ready", map[string]any{"protocol": 1, "heartbeatSeconds": authenticated.heartbeatSeconds})

	result := relayResult{HeartbeatSeconds: authenticated.heartbeatSeconds, RTTs: make([]time.Duration, 0, pingCount)}
	processStart := time.Now()
	for index := 0; index < pingCount; index++ {
		if index > 0 {
			timer := time.NewTimer(time.Duration(authenticated.heartbeatSeconds) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return relayResult{}, stageError{code: 50, err: ctx.Err()}
			case <-timer.C:
			}
		}
		monotonic := uint64(max(time.Since(processStart).Milliseconds(), 1))
		started := time.Now()
		if err := writeRelayEnvelope(ctx, connection, &remotev1.Envelope{
			ProtocolVersion: 1, ConnectionEpoch: state.ConnectionEpoch, Sequence: uint64(index + 1),
			Frame: &remotev1.Envelope_Ping{Ping: &remotev1.Ping{MonotonicMillis: monotonic}},
		}); err != nil {
			return relayResult{}, stageError{code: 50, err: fmt.Errorf("write Ping %d: %w", index+1, err)}
		}
		responseEnvelope, err := readRelayEnvelope(ctx, connection)
		if err != nil {
			return relayResult{}, stageError{code: 50, err: fmt.Errorf("wait for Pong %d: %w", index+1, err)}
		}
		if goAway := responseEnvelope.GetGoAway(); goAway != nil {
			return relayResult{}, stageError{code: 50, err: goAwayMessage(goAway)}
		}
		if err := validatePong(responseEnvelope, state.ConnectionEpoch, uint64(index+1), monotonic); err != nil {
			return relayResult{}, stageError{code: 50, err: fmt.Errorf("Pong %d does not match Ping", index+1)}
		}
		result.RTTs = append(result.RTTs, time.Since(started))
	}
	result.Min, result.Max = result.RTTs[0], result.RTTs[0]
	var total time.Duration
	for _, rtt := range result.RTTs {
		total += rtt
		if rtt < result.Min {
			result.Min = rtt
		}
		if rtt > result.Max {
			result.Max = rtt
		}
	}
	result.Average = total / time.Duration(len(result.RTTs))
	_ = connection.Close(websocket.StatusNormalClosure, "MVP Ping/Pong complete")
	return result, nil
}

func dialRelayEndpoint(ctx context.Context, client *http.Client, endpoint allocationEndpoint, assignmentID uuid.UUID, ticket string, state clientState) (authenticatedRelayConnection, error) {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.User != nil || parsed.Path != "/v1/connect" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return authenticatedRelayConnection{}, stageError{code: 40, err: errors.New("Relay endpoint is not a valid wss://.../v1/connect URL")}
	}
	header := make(http.Header)
	header.Set("Authorization", "Relay "+ticket)
	connection, response, err := websocket.Dial(ctx, parsed.String(), &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: header, Subprotocols: []string{relayserver.Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return authenticatedRelayConnection{}, stageError{code: 40, err: errors.New("WSS/TLS connection failed")}
	}
	fail := func(err error) (authenticatedRelayConnection, error) {
		connection.CloseNow()
		return authenticatedRelayConnection{}, err
	}
	if connection.Subprotocol() != relayserver.Subprotocol {
		return fail(stageError{code: 40, err: errors.New("Relay subprotocol negotiation failed")})
	}
	connection.SetReadLimit(1 << 20)

	challengeEnvelope, err := readRelayEnvelope(ctx, connection)
	if err != nil {
		return fail(stageError{code: 41, err: fmt.Errorf("read AuthChallenge: %w", err)})
	}
	if goAway := challengeEnvelope.GetGoAway(); goAway != nil {
		return fail(stageError{code: 41, err: goAwayMessage(goAway)})
	}
	challengeFrame := challengeEnvelope.GetAuthChallenge()
	if challengeEnvelope.GetProtocolVersion() != 1 || challengeFrame == nil || len(challengeFrame.GetNonce()) != 32 ||
		challengeFrame.GetRelayCellId() != endpoint.CellID.String() || challengeFrame.GetDeadline() == nil {
		return fail(stageError{code: 41, err: errors.New("AuthChallenge is invalid")})
	}
	deadline := challengeFrame.GetDeadline().AsTime().UTC()
	if !deadline.After(time.Now().UTC()) {
		return fail(stageError{code: 41, err: errors.New("AuthChallenge is expired")})
	}
	ticketClaims, err := parseTicketClaims(ticket)
	if err != nil || ticketClaims.JWTID == "" || ticketClaims.AssignmentID != assignmentID.String() {
		return fail(stageError{code: 41, err: errors.New("Relay Ticket claims are invalid")})
	}
	challenge := remoteauth.Challenge{
		Nonce: append([]byte(nil), challengeFrame.GetNonce()...), TicketJWTID: ticketClaims.JWTID,
		CellID: challengeFrame.GetRelayCellId(), NodeID: challengeFrame.GetRelayNodeId(),
		ConnectionEpoch: state.ConnectionEpoch, Deadline: deadline,
	}
	signature, err := remoteauth.SignChallenge(state.identity, challenge)
	if err != nil {
		return fail(stageError{code: 41, err: errors.New("AuthChallenge signing failed")})
	}
	if err := writeRelayEnvelope(ctx, connection, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: state.ConnectionEpoch,
		Frame: &remotev1.Envelope_AuthProof{AuthProof: &remotev1.AuthProof{
			TicketJti: ticketClaims.JWTID, ConnectionEpoch: state.ConnectionEpoch, DeviceSignature: signature,
		}},
	}); err != nil {
		return fail(stageError{code: 41, err: fmt.Errorf("write AuthProof: %w", err)})
	}
	readyEnvelope, err := readRelayEnvelope(ctx, connection)
	if err != nil {
		return fail(stageError{code: 41, err: fmt.Errorf("wait for Ready: %w", err)})
	}
	if goAway := readyEnvelope.GetGoAway(); goAway != nil {
		return fail(stageError{code: 41, err: goAwayMessage(goAway)})
	}
	ready := readyEnvelope.GetReady()
	if readyEnvelope.GetProtocolVersion() != 1 || ready == nil || ready.GetConnectionId() == "" ||
		ready.GetAcceptedConnectionEpoch() != state.ConnectionEpoch || readyEnvelope.GetConnectionEpoch() != state.ConnectionEpoch ||
		ready.GetHeartbeatIntervalSeconds() == 0 {
		return fail(stageError{code: 41, err: errors.New("Ready frame is invalid")})
	}
	return authenticatedRelayConnection{socket: connection, heartbeatSeconds: ready.GetHeartbeatIntervalSeconds()}, nil
}

func dialDirectPeerRelay(ctx context.Context, client *http.Client, ticket peerTicketResponse, state clientState) (authenticatedRelayConnection, error) {
	parsed, err := url.Parse(ticket.RelayURL)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.User != nil || parsed.Path != "/v1/connect" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		ticket.RelayNodeID == uuid.Nil || ticket.RelayCellID == uuid.Nil || ticket.PeerSessionTicket == "" {
		return authenticatedRelayConnection{}, stageError{code: 40, err: errors.New("direct Peer Relay endpoint is invalid")}
	}
	header := make(http.Header)
	header.Set("Authorization", "Peer "+ticket.PeerSessionTicket)
	connection, response, err := websocket.Dial(ctx, parsed.String(), &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: header, Subprotocols: []string{relayserver.Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return authenticatedRelayConnection{}, stageError{code: 40, err: errors.New("direct Peer WSS/TLS connection failed")}
	}
	fail := func(err error) (authenticatedRelayConnection, error) {
		connection.CloseNow()
		return authenticatedRelayConnection{}, err
	}
	if connection.Subprotocol() != relayserver.Subprotocol {
		return fail(stageError{code: 40, err: errors.New("Relay subprotocol negotiation failed")})
	}
	connection.SetReadLimit(1 << 20)
	challengeEnvelope, err := readRelayEnvelope(ctx, connection)
	if err != nil {
		return fail(stageError{code: 41, err: fmt.Errorf("read direct AuthChallenge: %w", err)})
	}
	challengeFrame := challengeEnvelope.GetAuthChallenge()
	if challengeEnvelope.GetProtocolVersion() != 1 || challengeFrame == nil || len(challengeFrame.GetNonce()) != 32 ||
		challengeFrame.GetRelayCellId() != ticket.RelayCellID.String() || challengeFrame.GetRelayNodeId() != ticket.RelayNodeID.String() ||
		challengeFrame.GetDeadline() == nil {
		return fail(stageError{code: 41, err: errors.New("direct AuthChallenge is invalid")})
	}
	deadline := challengeFrame.GetDeadline().AsTime().UTC()
	if !deadline.After(time.Now().UTC()) {
		return fail(stageError{code: 41, err: errors.New("direct AuthChallenge is expired")})
	}
	claims, err := parseTicketClaims(ticket.PeerSessionTicket)
	if err != nil || claims.JWTID == "" || claims.SessionID != ticket.SessionID.String() ||
		claims.RelayNodeID != ticket.RelayNodeID.String() || claims.RelayCellID != ticket.RelayCellID.String() {
		return fail(stageError{code: 41, err: errors.New("direct Peer Ticket claims are invalid")})
	}
	challenge := remoteauth.Challenge{
		Nonce: append([]byte(nil), challengeFrame.GetNonce()...), TicketJWTID: claims.JWTID,
		CellID: challengeFrame.GetRelayCellId(), NodeID: challengeFrame.GetRelayNodeId(),
		ConnectionEpoch: state.ConnectionEpoch, Deadline: deadline,
	}
	signature, err := remoteauth.SignChallenge(state.identity, challenge)
	if err != nil {
		return fail(stageError{code: 41, err: errors.New("direct AuthChallenge signing failed")})
	}
	if err := writeRelayEnvelope(ctx, connection, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: state.ConnectionEpoch,
		Frame: &remotev1.Envelope_AuthProof{AuthProof: &remotev1.AuthProof{
			TicketJti: claims.JWTID, ConnectionEpoch: state.ConnectionEpoch, DeviceSignature: signature,
		}},
	}); err != nil {
		return fail(stageError{code: 41, err: fmt.Errorf("write direct AuthProof: %w", err)})
	}
	readyEnvelope, err := readRelayEnvelope(ctx, connection)
	if err != nil {
		return fail(stageError{code: 41, err: fmt.Errorf("wait for direct Ready: %w", err)})
	}
	ready := readyEnvelope.GetReady()
	if readyEnvelope.GetProtocolVersion() != 1 || ready == nil || ready.GetConnectionId() == "" ||
		ready.GetAcceptedConnectionEpoch() != state.ConnectionEpoch || readyEnvelope.GetConnectionEpoch() != state.ConnectionEpoch ||
		ready.GetHeartbeatIntervalSeconds() == 0 {
		return fail(stageError{code: 41, err: errors.New("direct Ready frame is invalid")})
	}
	return authenticatedRelayConnection{socket: connection, heartbeatSeconds: ready.GetHeartbeatIntervalSeconds()}, nil
}

func validatePong(envelope *remotev1.Envelope, connectionEpoch, sequence, monotonicMillis uint64) error {
	if envelope == nil || connectionEpoch == 0 || sequence == 0 || monotonicMillis == 0 {
		return errors.New("Pong expectation is invalid")
	}
	pong := envelope.GetPong()
	if pong == nil || envelope.GetProtocolVersion() != 1 || envelope.GetConnectionEpoch() != connectionEpoch ||
		envelope.GetSequence() != sequence || pong.GetMonotonicMillis() != monotonicMillis {
		return errors.New("Pong does not match Ping")
	}
	return nil
}

func readRelayEnvelope(ctx context.Context, connection *websocket.Conn) (*remotev1.Envelope, error) {
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		return nil, err
	}
	if messageType != websocket.MessageBinary || len(payload) == 0 || len(payload) > 1<<20 {
		return nil, errors.New("Relay frame is not a bounded binary frame")
	}
	envelope := new(remotev1.Envelope)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		return nil, errors.New("Relay frame protobuf is invalid")
	}
	return envelope, nil
}

func writeRelayEnvelope(ctx context.Context, connection *websocket.Conn, envelope *remotev1.Envelope) error {
	payload, err := proto.Marshal(envelope)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageBinary, payload)
}

func parseTicketClaims(ticket string) (remoteauth.Claims, error) {
	parts := strings.Split(ticket, ".")
	if len(parts) != 3 {
		return remoteauth.Claims{}, errors.New("Ticket is malformed")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 16<<10 {
		return remoteauth.Claims{}, errors.New("Ticket claims are malformed")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var claims remoteauth.Claims
	if err := decoder.Decode(&claims); err != nil {
		return remoteauth.Claims{}, errors.New("Ticket claims are malformed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return remoteauth.Claims{}, errors.New("Ticket claims are malformed")
	}
	return claims, nil
}

func goAwayMessage(frame *remotev1.GoAway) error {
	return fmt.Errorf("Relay GoAway reason=%s refresh_assignment=%t reconnect_after=%s",
		frame.GetReason().String(), frame.GetRefreshAssignment(), time.Duration(frame.GetReconnectAfterMillis())*time.Millisecond)
}
