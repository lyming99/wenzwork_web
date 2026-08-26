package main

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCapabilityDiscoveryCanReuseBoundProjectSession(t *testing.T) {
	projectID := uuid.NewString()
	dispatch := dispatcher{
		scope:                 "remote.peer.ai.chat",
		ticketProjectID:       projectID,
		enforceProjectBinding: true,
	}
	if !methodAllowsScope("agent.capabilities.get", dispatch.scope) {
		t.Fatal("capability discovery must be allowed by an AI project session")
	}
	if err := dispatch.validateProjectBinding("agent.capabilities.get", projectID); err != nil {
		t.Fatalf("bound capability discovery = %v", err)
	}
	if err := dispatch.validateProjectBinding("agent.capabilities.get", uuid.NewString()); !errors.Is(err, errRPCProject) {
		t.Fatalf("mismatched capability discovery = %v, want project mismatch", err)
	}
	if methodAllowsScope("agent.capabilities.get", "remote.task.read") {
		t.Fatal("capability discovery must not be allowed by a non-Peer task scope")
	}
}

func TestProjectDiscoveryAndRefreshUsePeerQueryScope(t *testing.T) {
	for _, method := range []string{"project.list", "project.refresh", "project.sync", "project.remove"} {
		if got := methodScope(method); got != "remote.peer.query" {
			t.Errorf("methodScope(%q) = %q, want remote.peer.query", method, got)
		}
		if !methodAllowsScope(method, "remote.peer.query") {
			t.Errorf("Peer query scope must allow %q", method)
		}
	}
}
