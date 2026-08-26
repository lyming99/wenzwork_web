package main

import "time"

// deviceConnectionDiagnostic is a content-free Relay lifecycle record. It is
// deliberately limited to local state and timing: it never contains a device
// id, URL, ticket, access key, session id, raw error, or payload.
type deviceConnectionDiagnostic struct {
	OccurredAt             time.Time
	Event                  string
	Reason                 string
	ConnectionEpoch        uint64
	ReconnectAttempt       int
	RetryAfterMilliseconds int64
	HeartbeatMilliseconds  int64
}

func (state *agentState) recordConnectionDiagnostic(
	event, reason string,
	connectionEpoch uint64,
	reconnectAttempt int,
	retryAfter, heartbeat time.Duration,
) {
	if state == nil || event == "" || reason == "" {
		return
	}
	if connectionEpoch == 0 {
		state.mu.RLock()
		connectionEpoch = state.ConnectionEpoch
		state.mu.RUnlock()
	}
	diagnostic := deviceConnectionDiagnostic{
		OccurredAt:             time.Now().UTC(),
		Event:                  event,
		Reason:                 reason,
		ConnectionEpoch:        connectionEpoch,
		ReconnectAttempt:       reconnectAttempt,
		RetryAfterMilliseconds: retryAfter.Milliseconds(),
		HeartbeatMilliseconds:  heartbeat.Milliseconds(),
	}
	state.connectionDiagnosticMu.Lock()
	sink := state.connectionDiagnosticSink
	state.connectionDiagnosticMu.Unlock()
	if sink != nil {
		sink(diagnostic)
	}
}
