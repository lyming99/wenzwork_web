package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

func TestTargetRelayLoopReallocatesAndRebuildsTrustAfterInvalidBundleAndDisconnect(t *testing.T) {
	root := t.TempDir()
	agent, err := loadOrCreateAgentState(filepath.Join(root, "agent.json"), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	var diagnosticsMu sync.Mutex
	diagnostics := make([]deviceConnectionDiagnostic, 0, 16)
	agent.connectionDiagnosticSink = func(diagnostic deviceConnectionDiagnostic) {
		diagnosticsMu.Lock()
		diagnostics = append(diagnostics, diagnostic)
		diagnosticsMu.Unlock()
	}
	store, err := loadControlState(agent)
	if err != nil {
		t.Fatal(err)
	}
	publicTwo, _, _ := ed25519.GenerateKey(rand.Reader)
	publicThree, _, _ := ed25519.GenerateKey(rand.Reader)
	cellID := uuid.New()
	var mu sync.Mutex
	allocationCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/device/relay-allocations" || request.Header.Get("Authorization") != "Bearer relay-loop-access" {
			http.NotFound(writer, request)
			return
		}
		mu.Lock()
		allocationCalls++
		call := allocationCalls
		mu.Unlock()
		bundle := peerTicketTrustBundle{Issuer: "control.test"}
		switch call {
		case 2:
			bundle.Keys = []peerTicketTrustKey{{KeyID: "key-2", Algorithm: "Ed25519", PublicKey: base64.RawURLEncoding.EncodeToString(publicTwo)}}
		case 3:
			bundle.Keys = []peerTicketTrustKey{{KeyID: "key-3", Algorithm: "Ed25519", PublicKey: base64.RawURLEncoding.EncodeToString(publicThree)}}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(allocationResponse{
			AssignmentID: uuid.New(), Primary: allocationEndpoint{CellID: cellID, URL: "wss://relay.example.test/v1/connect"},
			ConnectionTicket: "opaque-ticket", TicketExpiresAt: time.Now().Add(2 * time.Second), PeerTicketTrust: bundle,
		})
	}))
	defer server.Close()
	manager, err := newDeviceTokenManager(server.Client(), mustURL(t, server.URL), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.acceptInitial(deviceTokenSet{AccessToken: "relay-loop-access", ExpiresIn: 600, RefreshToken: "relay-loop-refresh", RefreshExpiresIn: 1200, SessionID: uuid.New(), Scope: "remote.connect"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialCalls, serveCalls, closeCalls := 0, 0, 0
	hooks := targetRelayLoopHooks{
		dial: func(_ context.Context, _ *http.Client, allocation allocationResponse, _ *agentState) (*relayConnection, time.Duration, error) {
			dialCalls++
			if len(allocation.PeerTicketTrust.Keys) != 1 {
				t.Fatal("dial received invalid trust allocation")
			}
			return &relayConnection{}, time.Second, nil
		},
		serve: func(serveContext context.Context, _ *relayConnection, _ time.Duration, _ *agentState, verifier remoteauth.Verifier) error {
			serveCalls++
			// The allocation ticket only admits the handshake. The established
			// resident connection must retain the caller's lifecycle instead of
			// receiving a new deadline at the two-second ticket expiry.
			if deadline, ok := serveContext.Deadline(); !ok || time.Until(deadline) < 3*time.Second {
				t.Fatalf("serve context deadline = %v, present=%t", deadline, ok)
			}
			expectedKey := "key-2"
			if serveCalls == 2 {
				expectedKey = "key-3"
			}
			if len(verifier.Keys) != 1 || verifier.Keys[expectedKey] == nil {
				t.Fatalf("serve verifier keys = %#v, want only %s", verifier.Keys, expectedKey)
			}
			if serveCalls == 1 {
				return errors.New("simulated Relay disconnect")
			}
			cancel()
			return context.Canceled
		},
		close: func(*relayConnection) { closeCalls++ },
	}
	err = runTargetRelayLoopWithHooks(ctx, server.Client(), manager, agent, hooks)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Relay loop exit = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if allocationCalls != 3 || dialCalls != 2 || serveCalls != 2 || closeCalls != 2 || agent.ConnectionEpoch != 3 {
		t.Fatalf("allocation/dial/serve/close/epoch = %d/%d/%d/%d/%d", allocationCalls, dialCalls, serveCalls, closeCalls, agent.ConnectionEpoch)
	}
	diagnosticsMu.Lock()
	capturedDiagnostics := append([]deviceConnectionDiagnostic(nil), diagnostics...)
	diagnosticsMu.Unlock()
	hasDiagnostic := func(event, reason string) bool {
		for _, diagnostic := range capturedDiagnostics {
			if diagnostic.Event == event && diagnostic.Reason == reason {
				return true
			}
		}
		return false
	}
	if !hasDiagnostic("relay_allocation_requested", "requested") ||
		!hasDiagnostic("relay_allocation_invalid", "peer_ticket_trust_invalid") ||
		!hasDiagnostic("relay_connected", "ready") ||
		!hasDiagnostic("relay_disconnected", "transport_error") ||
		!hasDiagnostic("relay_reconnect_scheduled", "peer_ticket_trust_invalid") {
		t.Fatalf("connection diagnostics = %+v", capturedDiagnostics)
	}
}

func TestTargetRelayHeartbeatTimeoutToleratesOneMissedInterval(t *testing.T) {
	if got := targetRelayHeartbeatTimeout(25 * time.Second); got != 53*time.Second {
		t.Fatalf("heartbeat timeout = %s", got)
	}
	if got := targetRelayHeartbeatTimeout(time.Second); got != 15*time.Second {
		t.Fatalf("minimum heartbeat timeout = %s", got)
	}
}
