package relayserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"google.golang.org/protobuf/proto"
)

type fakeFrameConnection struct {
	writes chan []byte
	mu     sync.Mutex
	closed bool
}

func newFakeFrameConnection() *fakeFrameConnection {
	return &fakeFrameConnection{writes: make(chan []byte, 8)}
}

func (connection *fakeFrameConnection) Write(_ context.Context, _ websocket.MessageType, payload []byte) error {
	connection.writes <- append([]byte(nil), payload...)
	return nil
}

func (connection *fakeFrameConnection) Close(websocket.StatusCode, string) error {
	connection.mu.Lock()
	connection.closed = true
	connection.mu.Unlock()
	return nil
}

func TestConnectionManagerFencesOlderSessionAndDrains(t *testing.T) {
	manager, err := NewConnectionManager(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	firstConnection := newFakeFrameConnection()
	first, err := manager.Attach("device-1", "connection-1", 1, firstConnection)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Enqueue(&remotev1.Envelope{ProtocolVersion: 1, ConnectionEpoch: 1, Frame: &remotev1.Envelope_Ready{Ready: &remotev1.Ready{}}}); err != nil {
		t.Fatal(err)
	}
	readWrittenEnvelope(t, firstConnection)

	secondConnection := newFakeFrameConnection()
	if _, err := manager.Attach("device-1", "connection-2", 2, secondConnection); err != nil {
		t.Fatal(err)
	}
	goAway := readWrittenEnvelope(t, firstConnection).GetGoAway()
	if goAway == nil || !goAway.GetRefreshAssignment() {
		t.Fatalf("superseded GOAWAY = %+v", goAway)
	}
	if _, err := manager.Attach("device-1", "connection-stale", 1, newFakeFrameConnection()); err == nil {
		t.Fatal("Attach accepted a stale connection epoch")
	}

	manager.BeginDrain(remotev1.GoAwayReason_GO_AWAY_REASON_NODE_DRAINING, time.Second, false, time.Second)
	drain := readWrittenEnvelope(t, secondConnection).GetGoAway()
	if drain == nil || drain.GetReason() != remotev1.GoAwayReason_GO_AWAY_REASON_NODE_DRAINING {
		t.Fatalf("drain GOAWAY = %+v", drain)
	}
	if manager.Accepting() {
		t.Fatal("draining manager still accepts connections")
	}
	drainGeneration := manager.drainGeneration.Load()
	manager.Resume()
	if !manager.Accepting() || manager.drainGeneration.Load() <= drainGeneration {
		t.Fatal("resumed manager did not accept connections or fence the old drain deadline")
	}
	metrics := manager.Metrics()
	if metrics.Active != 1 || metrics.Peak != 1 || metrics.SupersededSessions != 1 || metrics.DrainStarted != 1 {
		t.Fatalf("connection metrics = %+v", metrics)
	}
	manager.CloseAll(websocket.StatusGoingAway, "test complete")
}

func TestConnectionManagerRevocationIsTerminal(t *testing.T) {
	manager, err := NewConnectionManager(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	connection := newFakeFrameConnection()
	if _, err := manager.Attach("device-1", "connection-1", 1, connection); err != nil {
		t.Fatal(err)
	}

	manager.Revoke()
	goAway := readWrittenEnvelope(t, connection).GetGoAway()
	if goAway == nil || goAway.GetReason() != remotev1.GoAwayReason_GO_AWAY_REASON_GRANT_REVOKED || !goAway.GetRefreshAssignment() {
		t.Fatalf("revocation GOAWAY = %+v", goAway)
	}
	manager.Resume()
	if manager.Accepting() {
		t.Fatal("revoked manager resumed admission")
	}
	if err := manager.AcquireHandshake(context.Background()); !errors.Is(err, ErrNodeDraining) {
		t.Fatalf("revoked AcquireHandshake() error = %v", err)
	}
	manager.CloseAll(websocket.StatusPolicyViolation, "test complete")
}

func TestConnectionManagerReportsAndAppliesHostRouteDecision(t *testing.T) {
	manager, err := NewConnectionManager(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	connection := newFakeFrameConnection()
	if _, err := manager.Attach("device-1", "connection-1", 7, connection); err != nil {
		t.Fatal(err)
	}
	if err := manager.BindResidentRoute("device-1", "connection-1", 7, relayrouter.Route{
		DeviceID: "device-1", UserID: "user-1", CellID: "cell-1", NodeID: "node-1",
		ConnectionID: "connection-1", ConnectionEpoch: 7, AssignmentVersion: 4, GrantVersion: 3, ProtocolVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.RouteChanges():
	case <-time.After(time.Second):
		t.Fatal("route bind did not wake the heartbeat")
	}
	routes := manager.ResidentRoutes()
	if len(routes) != 1 || routes[0].DeviceID != "device-1" || routes[0].ConnectionEpoch != 7 || routes[0].GrantVersion != 3 {
		t.Fatalf("resident routes = %+v", routes)
	}
	if !manager.RejectResident("device-1", "connection-1", 7, remotev1.GoAwayReason_GO_AWAY_REASON_GRANT_REVOKED) {
		t.Fatal("Host route rejection was not applied")
	}
	goAway := readWrittenEnvelope(t, connection).GetGoAway()
	if goAway == nil || goAway.GetReason() != remotev1.GoAwayReason_GO_AWAY_REASON_GRANT_REVOKED || !goAway.GetRefreshAssignment() {
		t.Fatalf("Host rejection GOAWAY = %+v", goAway)
	}
	if routes := manager.ResidentRoutes(); len(routes) != 0 {
		t.Fatalf("routes after rejection = %+v", routes)
	}
}

func readWrittenEnvelope(t *testing.T, connection *fakeFrameConnection) *remotev1.Envelope {
	t.Helper()
	select {
	case payload := <-connection.writes:
		var envelope remotev1.Envelope
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		return &envelope
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Relay frame")
		return nil
	}
}
