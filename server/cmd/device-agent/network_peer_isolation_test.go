package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

func TestServeTargetPeerRejectsUnknownSessionWithoutEndingRelay(t *testing.T) {
	serveContext, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	served := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		socket, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer socket.CloseNow()
		served <- serveTargetPeer(serveContext, &relayConnection{socket: socket, epoch: 7}, time.Hour, &agentState{}, remoteauth.Verifier{})
	}))
	defer server.Close()

	client, response, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial status=%d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	defer client.CloseNow()

	for index := 0; index < 2; index++ {
		sessionID, queryID := uuid.NewString(), uuid.NewString()
		writeTargetPeerTestEnvelope(t, client, &remotev1.Envelope{
			ProtocolVersion: 1, ConnectionEpoch: 7,
			Frame: &remotev1.Envelope_PeerQuery{PeerQuery: &remotev1.PeerCiphertext{
				SessionId: sessionID, QueryId: queryID,
			}},
		})
		response := readTargetPeerTestEnvelope(t, client)
		peerError := response.GetPeerError()
		if peerError == nil || peerError.GetSessionId() != sessionID || peerError.GetQueryId() != queryID ||
			peerError.GetCode() != remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED {
			t.Fatalf("unknown session response = %+v", peerError)
		}
	}

	// Receiving a second scoped rejection proves the read loop stayed alive;
	// it must not turn an unknown logical session into a physical reconnect.
	select {
	case err := <-served:
		t.Fatalf("Relay ended after unknown session: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func writeTargetPeerTestEnvelope(t *testing.T, socket *websocket.Conn, envelope *remotev1.Envelope) {
	t.Helper()
	payload, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := socket.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
}

func readTargetPeerTestEnvelope(t *testing.T, socket *websocket.Conn) *remotev1.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	messageType, payload, err := socket.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageBinary {
		t.Fatalf("message type = %v", messageType)
	}
	envelope := new(remotev1.Envelope)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}
