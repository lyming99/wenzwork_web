package relayprotocol

import (
	"errors"
	"testing"
	"time"
)

func TestAgentGOAWAYReconnectsWithGreaterPersistentEpoch(t *testing.T) {
	agent := NewAgentRelayState(40)
	firstEpoch, err := agent.BeginConnect()
	if err != nil || firstEpoch != 41 {
		t.Fatalf("BeginConnect() = %d, %v", firstEpoch, err)
	}
	if err := agent.MarkReady(firstEpoch); err != nil {
		t.Fatal(err)
	}
	instruction, err := agent.HandleGoAway(time.Second, true)
	if err != nil || !instruction.RefreshAssignment {
		t.Fatalf("HandleGoAway() = %+v, %v", instruction, err)
	}
	secondEpoch, err := agent.BeginConnect()
	if err != nil || secondEpoch != 42 {
		t.Fatalf("second BeginConnect() = %d, %v", secondEpoch, err)
	}
	if err := agent.MarkReady(firstEpoch); !errors.Is(err, ErrAgentState) {
		t.Fatalf("old READY error = %v", err)
	}
	if err := agent.MarkReady(secondEpoch); err != nil {
		t.Fatal(err)
	}
}
