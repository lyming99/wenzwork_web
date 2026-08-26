package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const maximumDeviceProtocolDiagnostics = 128

// deviceProtocolDiagnostic deliberately contains no raw method, identifier,
// payload, URL, token, ticket, key, or error string. It is safe to retain in
// memory and to project into structured telemetry.
type deviceProtocolDiagnostic struct {
	OccurredAt        time.Time `json:"occurredAt"`
	ConnectionEpoch   uint64    `json:"connectionEpoch"`
	Stage             string    `json:"stage"`
	Reason            string    `json:"reason"`
	FaultLevel        string    `json:"faultLevel"`
	Direction         string    `json:"direction"`
	MethodClass       string    `json:"methodClass"`
	Scope             string    `json:"scope"`
	PayloadSizeBucket string    `json:"payloadSizeBucket"`
	RequestHash       string    `json:"requestHash,omitempty"`
	SessionHash       string    `json:"sessionHash,omitempty"`
	RootFailureID     string    `json:"rootFailureId"`
}

func (state *agentState) recordProtocolDiagnostic(stage, reason, faultLevel, direction, method, scope string, payloadBytes int, requestID, sessionID string) {
	if state == nil || stage == "" || reason == "" || faultLevel == "" {
		return
	}
	state.mu.RLock()
	epoch := state.ConnectionEpoch
	state.mu.RUnlock()

	state.protocolDiagnosticsMu.Lock()
	state.initializeProtocolDiagnosticSaltLocked()
	requestHash := state.protocolIdentifierHashLocked("request", requestID)
	sessionHash := state.protocolIdentifierHashLocked("session", sessionID)
	root := strings.Join([]string{stage, reason, faultLevel, strconv.FormatUint(epoch, 10), scope, requestHash, sessionHash}, "|")
	diagnostic := deviceProtocolDiagnostic{
		OccurredAt: time.Now().UTC(), ConnectionEpoch: epoch, Stage: stage, Reason: reason,
		FaultLevel: faultLevel, Direction: safeProtocolDimension(direction), MethodClass: protocolMethodClass(method),
		Scope: safeProtocolScope(scope), PayloadSizeBucket: protocolPayloadSizeBucket(payloadBytes),
		RequestHash: requestHash, SessionHash: sessionHash, RootFailureID: state.protocolIdentifierHashLocked("root", root),
	}
	state.protocolDiagnostics = append(state.protocolDiagnostics, diagnostic)
	if len(state.protocolDiagnostics) > maximumDeviceProtocolDiagnostics {
		copy(state.protocolDiagnostics, state.protocolDiagnostics[len(state.protocolDiagnostics)-maximumDeviceProtocolDiagnostics:])
		state.protocolDiagnostics = state.protocolDiagnostics[:maximumDeviceProtocolDiagnostics]
	}
	sink := state.protocolDiagnosticSink
	state.protocolDiagnosticsMu.Unlock()
	if sink != nil {
		sink(diagnostic)
	}
}

func (state *agentState) protocolDiagnosticSnapshot() []deviceProtocolDiagnostic {
	if state == nil {
		return nil
	}
	state.protocolDiagnosticsMu.Lock()
	defer state.protocolDiagnosticsMu.Unlock()
	return append([]deviceProtocolDiagnostic(nil), state.protocolDiagnostics...)
}

func (state *agentState) initializeProtocolDiagnosticSaltLocked() {
	if state.protocolDiagnosticSalt != ([32]byte{}) {
		return
	}
	if _, err := rand.Read(state.protocolDiagnosticSalt[:]); err == nil {
		return
	}
	state.protocolDiagnosticSalt = sha256.Sum256([]byte("wenzwork-device-protocol-diagnostics:" + state.DeviceID.String()))
}

func (state *agentState) protocolIdentifierHashLocked(kind, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	digest := hmac.New(sha256.New, state.protocolDiagnosticSalt[:])
	_, _ = digest.Write([]byte(kind))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(value))
	return hex.EncodeToString(digest.Sum(nil)[:12])
}

func protocolMethodClass(method string) string {
	method = strings.TrimSpace(method)
	for _, prefix := range []string{"agent.", "project.", "file.", "terminal.", "task.", "workflow.", "conversation.", "chat.", "event.", "rpc."} {
		if strings.HasPrefix(method, prefix) {
			return strings.TrimSuffix(prefix, ".")
		}
	}
	return "unknown"
}

func safeProtocolScope(scope string) string {
	switch scope {
	case "remote.peer.query", "remote.peer.file.send", "remote.peer.file.receive", "remote.peer.terminal",
		"remote.peer.terminal.interactive", "remote.peer.task.control", "remote.peer.ai.config", "remote.peer.ai.chat",
		"remote.peer.ai.tools", "remote.peer.events":
		return scope
	default:
		return "unknown"
	}
}

func safeProtocolDimension(value string) string {
	switch value {
	case "inbound", "outbound", "local":
		return value
	default:
		return "unknown"
	}
}

func protocolPayloadSizeBucket(size int) string {
	if size <= 0 {
		return "empty"
	}
	return rpcPayloadSizeBucket(size)
}
