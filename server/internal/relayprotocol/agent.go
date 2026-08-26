package relayprotocol

import (
	"errors"
	"time"
)

var ErrAgentState = errors.New("invalid Agent Relay state")

type AgentConnectionState string

const (
	AgentDisconnected AgentConnectionState = "disconnected"
	AgentConnecting   AgentConnectionState = "connecting"
	AgentReady        AgentConnectionState = "ready"
	AgentReconnecting AgentConnectionState = "reconnecting"
)

type ReconnectInstruction struct {
	RefreshAssignment bool
	ReconnectAfter    time.Duration
}

// AgentRelayState models the connection_epoch value that the desktop Agent
// persists in SQLite. A real Socket is never migrated; GOAWAY coordinates a
// new connection with a strictly greater epoch.
type AgentRelayState struct {
	State           AgentConnectionState
	ConnectionEpoch uint64
}

func NewAgentRelayState(persistedEpoch uint64) *AgentRelayState {
	return &AgentRelayState{State: AgentDisconnected, ConnectionEpoch: persistedEpoch}
}

func (s *AgentRelayState) BeginConnect() (uint64, error) {
	if s.State != AgentDisconnected && s.State != AgentReconnecting {
		return 0, ErrAgentState
	}
	s.ConnectionEpoch++
	if s.ConnectionEpoch == 0 {
		return 0, ErrAgentState
	}
	s.State = AgentConnecting
	return s.ConnectionEpoch, nil
}

func (s *AgentRelayState) MarkReady(acceptedEpoch uint64) error {
	if s.State != AgentConnecting || acceptedEpoch != s.ConnectionEpoch {
		return ErrAgentState
	}
	s.State = AgentReady
	return nil
}

func (s *AgentRelayState) HandleGoAway(reconnectAfter time.Duration, refreshAssignment bool) (ReconnectInstruction, error) {
	if s.State != AgentReady || reconnectAfter < 0 || reconnectAfter > 5*time.Minute {
		return ReconnectInstruction{}, ErrAgentState
	}
	s.State = AgentReconnecting
	return ReconnectInstruction{RefreshAssignment: refreshAssignment, ReconnectAfter: reconnectAfter}, nil
}

func (s *AgentRelayState) ConnectionLost() {
	if s.State == AgentReady || s.State == AgentConnecting {
		s.State = AgentReconnecting
	}
}
