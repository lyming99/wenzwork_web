package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	peerv2 "github.com/wenzwork/wenzwork-web/server/internal/peerprotocol/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/relayserver"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestV2RelayAndDeviceAgentKeepCapabilityQueryDeviceScoped exercises the
// production v2 boundaries instead of merely unit-testing a scope predicate:
// a real Relay forwards a signed Link handshake to a real Agent, then the
// controller opens first a device query Channel and next a project AI Channel.
// The Agent's dispatcher only accepts the latter if the encrypted RPC header
// carries exactly that project's ID.
func TestV2RelayAndDeviceAgentKeepCapabilityQueryDeviceScoped(t *testing.T) {
	previousHeartbeat := v2EventHeartbeatInterval
	v2EventHeartbeatInterval = 2 * time.Minute
	t.Cleanup(func() { v2EventHeartbeatInterval = previousHeartbeat })
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	state, err := loadOrCreateAgentState(filepath.Join(t.TempDir(), "state.json"), filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	controlStore, err := loadControlState(state)
	if err != nil {
		t.Fatal(err)
	}
	state.controlStore = controlStore
	state.ConnectionEpoch = 1
	workspace := t.TempDir()
	project, err := state.business.addProject(ctx, workspace, "v2 integration", "", defaultProjectPolicy)
	if err != nil {
		t.Fatal(err)
	}
	defer setInteractiveTerminalRuntimeProbe(func() bool { return true })()
	terminalStarter := new(fakePTYStarter)
	terminalSupervisor := newProcessSupervisorWithDependencies(terminalStarter, func(int) (uint64, error) { return 0, nil }, maximumTerminalSessions)
	terminalService := newTerminalService(state, terminalSupervisor)
	terminalService.shellArgv = func(string) ([]string, string, error) {
		return []string{filepath.Join(workspace, "fake-shell")}, "fake", nil
	}
	state.servicesMu.Lock()
	state.processes, state.terminals = terminalSupervisor, terminalService
	state.servicesMu.Unlock()
	aiConfig := installTestAIConfig(state)
	aiConversation, err := state.business.createAIConversation(ctx, project.ID, "", "v2 rekey", "readOnly", aiConfig, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	aiTurn, err := state.business.beginAIConversationTurn(ctx, project.ID, aiConversation.ID, uuid.NewString(), "keep this generation attached", "readOnly", nil, aiConfig, time.Now().UTC().Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	aiHighWatermark, _, err := state.business.aiConversationEventWatermarks(ctx, project.ID, aiConversation.ID)
	if err != nil {
		t.Fatalf("initial AI event watermark = %d, %v", aiHighWatermark, err)
	}
	initialProjectEvent := appendV2E2EAgentEvent(t, ctx, state, agentEventRecord{
		ProjectID: project.ID, Topic: "agent", EventType: "agent.status.changed", AggregateType: "agent", AggregateID: state.DeviceID.String(),
		Operation: "status", Revision: state.Revision, Cursor: agentEventCursor{Kind: "agent_status", Value: 1},
		Data: map[string]any{"status": "ready", "activeTaskCount": 0, "activeGenerationCount": 1}, OccurredAt: time.Now().UTC(),
	})
	if initialProjectEvent.Sequence == 0 {
		t.Fatal("initial project Event did not receive a durable sequence")
	}
	const downloadBytes = 100 << 20
	downloadPayload := bytes.Repeat([]byte("remote-v2-data!!"), downloadBytes/len("remote-v2-data!!"))
	if len(downloadPayload) != downloadBytes {
		t.Fatalf("download payload = %d bytes, want %d", len(downloadPayload), downloadBytes)
	}
	if err := os.WriteFile(filepath.Join(workspace, "v2-download.bin"), downloadPayload, 0o600); err != nil {
		t.Fatal(err)
	}

	_, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, grantPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientID, userID, nodeID, cellID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	claims := remoteauth.DeviceLinkGrantClaims{
		Audience:                 remoteauth.DeviceLinkGrantAudience,
		GrantID:                  uuid.NewString(),
		ClientID:                 clientID,
		DeviceID:                 state.DeviceID.String(),
		RelayNodeID:              nodeID,
		RelayCellID:              cellID,
		TargetConnectionEpoch:    state.ConnectionEpoch,
		ClientIdentityKey:        base64.RawURLEncoding.EncodeToString(clientPrivate.Public().(ed25519.PublicKey)),
		ClientKeyThumbprint:      remoteauth.PublicKeyThumbprint(clientPrivate.Public().(ed25519.PublicKey)),
		ClientIdentityKeyVersion: 1,
		DeviceKeyThumbprint:      remoteauth.PublicKeyThumbprint(state.identity.Public().(ed25519.PublicKey)),
		DeviceIdentityKeyVersion: state.KeyVersion,
		ClientGrantVersion:       1,
		DeviceGrantVersion:       1,
		AllowedScopes:            []string{"remote.peer.query", "remote.peer.ai.chat", "remote.peer.events", "remote.peer.file.send", "remote.peer.file.receive", "remote.peer.task.control", "remote.peer.terminal", "remote.peer.terminal.interactive"},
		MaximumLifetimeSeconds:   90,
		IssuedAt:                 now.Unix(),
		NotBefore:                now.Add(-time.Second).Unix(),
		ExpiresAt:                now.Add(90 * time.Second).Unix(),
	}
	grant, err := (remoteauth.DeviceLinkGrantIssuer{Issuer: "v2-e2e", KeyID: "v2-e2e", PrivateKey: grantPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	verifier := remoteauth.DeviceLinkGrantVerifier{
		Issuer: "v2-e2e",
		Keys:   map[string]ed25519.PublicKey{"v2-e2e": grantPrivate.Public().(ed25519.PublicKey)},
	}
	handler := &relayserver.V2Handler{
		CellID:              cellID,
		NodeID:              nodeID,
		ClientGrantVerifier: verifier,
		DeviceAuthenticator: v2E2EDeviceAuthenticator{
			deviceID: state.DeviceID.String(), userID: userID, epoch: state.ConnectionEpoch,
		},
		GrantUses: relayserver.NewInMemoryV2GrantUseStore(),
		BrowserOriginPatterns: []string{
			"*",
		},
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v2/connect"

	deviceSocket, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{Subprotocols: []string{relayserver.V2Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	deviceSocket.SetReadLimit(v2AgentMaximumCarrierFrame)
	deviceCarrier, err := newV2AgentCarrier(deviceSocket, uuid.NewString(), state.ConnectionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	links := newV2AgentLinkRegistry(state)
	deviceContext, stopDevice := context.WithCancel(ctx)
	deviceDone := make(chan error, 1)
	if err := deviceCarrier.send(ctx, &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
		DeviceConnectionTicket: "test-device-ticket",
		DeviceId:               state.DeviceID.String(),
		DeviceConnectionEpoch:  state.ConnectionEpoch,
		ClientChallenge:        randomV2E2EBytes(t, 32),
		DeviceProof:            randomV2E2EBytes(t, ed25519.SignatureSize),
	}}}, v2AgentControl); err != nil {
		t.Fatal(err)
	}
	deviceReady, err := readV2AgentEnvelope(ctx, deviceSocket)
	if err != nil || deviceCarrier.acceptIncoming(deviceReady) != nil || deviceReady.GetReady() == nil ||
		deviceReady.GetReady().GetCarrierId() != deviceCarrier.id || deviceReady.GetReady().GetCarrierEpoch() != state.ConnectionEpoch {
		t.Fatalf("device CarrierReady = %#v, %v", deviceReady, err)
	}
	go func() { deviceDone <- serveTargetV2(deviceContext, deviceCarrier, time.Hour, links, verifier) }()
	defer func() {
		stopDevice()
		deviceCarrier.close()
		select {
		case <-deviceDone:
		case <-time.After(2 * time.Second):
			t.Error("v2 Agent carrier did not stop")
		}
	}()
	controllerSocket, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{Subprotocols: []string{relayserver.V2Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	controllerSocket.SetReadLimit(v2AgentMaximumCarrierFrame)
	defer controllerSocket.CloseNow()
	controller := &v2E2EController{t: t, ctx: ctx, socket: controllerSocket, carrierID: uuid.NewString(), epoch: 1, keyID: 1}
	challenge := randomV2E2EBytes(t, 32)
	proof, err := remoteauth.SignCarrierProof(clientPrivate, remoteauth.CarrierProof{
		GrantID: claims.GrantID, CarrierID: controller.carrierID, CarrierEpoch: controller.epoch, Challenge: challenge,
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.send(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
		Grant:                    grant,
		GrantId:                  claims.GrantID,
		ClientId:                 clientID,
		ClientIdentityKeyVersion: 1,
		ClientChallenge:          challenge,
		ClientProof:              proof,
	}}})
	if ready := controller.next(); ready.GetReady() == nil {
		t.Fatalf("client CarrierReady = %#v", ready)
	}

	linkID := uuid.NewString()
	controller.linkID = linkID
	clientEphemeral, err := peerv2.GenerateEphemeralKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer peerv2.ClearPrivateKey(&clientEphemeral)
	initialBinding := peerv2.HandshakeBinding{
		GrantID:               claims.GrantID,
		LinkID:                linkID,
		ClientID:              clientID,
		DeviceID:              state.DeviceID.String(),
		RelayNodeID:           nodeID,
		RelayCellID:           cellID,
		TargetConnectionEpoch: state.ConnectionEpoch,
		ClientIdentityVersion: 1,
		ClientEphemeralPublic: clientEphemeral.PublicKey().Bytes(),
		ClientChallenge:       randomV2E2EBytes(t, 32),
		ExpiresAtUnixMilli:    time.Unix(claims.ExpiresAt, 0).UTC().UnixMilli(),
	}
	initSignature, err := peerv2.SignLinkInit(clientPrivate, initialBinding)
	if err != nil {
		t.Fatal(err)
	}
	controller.send(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Link{Link: &remotev2.LinkEnvelope{
		LinkId: linkID,
		Body: &remotev2.LinkEnvelope_LinkInit{LinkInit: &remotev2.LinkInit{
			GrantId:                  claims.GrantID,
			LinkId:                   linkID,
			ClientId:                 clientID,
			DeviceId:                 state.DeviceID.String(),
			RelayNodeId:              nodeID,
			RelayCellId:              cellID,
			TargetConnectionEpoch:    state.ConnectionEpoch,
			ClientIdentityKeyVersion: 1,
			ClientEphemeralPublicKey: initialBinding.ClientEphemeralPublic,
			ClientChallenge:          initialBinding.ClientChallenge,
			ExpiresAt:                timestamppb.New(time.Unix(claims.ExpiresAt, 0).UTC()),
			IdentitySignature:        initSignature,
			DeviceConnectionGrant:    grant,
		}},
	}}})

	acceptEnvelope := controller.next()
	accept := acceptEnvelope.GetLink().GetLinkAccept()
	if accept == nil {
		t.Fatalf("LinkAccept = %#v", acceptEnvelope)
	}
	fullBinding := initialBinding
	fullBinding.DeviceIdentityVersion = accept.GetDeviceIdentityKeyVersion()
	fullBinding.DeviceEphemeralPublic = append([]byte(nil), accept.GetDeviceEphemeralPublicKey()...)
	fullBinding.DeviceChallenge = append([]byte(nil), accept.GetDeviceChallenge()...)
	if accept.GetClientId() != clientID || accept.GetDeviceId() != state.DeviceID.String() ||
		accept.GetGrantId() != claims.GrantID || accept.GetLinkId() != linkID || accept.GetRelayNodeId() != nodeID || accept.GetRelayCellId() != cellID ||
		accept.GetTargetConnectionEpoch() != state.ConnectionEpoch || !accept.GetExpiresAt().AsTime().Equal(time.Unix(claims.ExpiresAt, 0).UTC()) ||
		peerv2.VerifyLinkAccept(state.identity.Public().(ed25519.PublicKey), fullBinding, accept.GetIdentitySignature()) != nil {
		t.Fatalf("LinkAccept binding is invalid: %#v", accept)
	}
	shared, err := peerv2.X25519SharedSecret(clientEphemeral, fullBinding.DeviceEphemeralPublic)
	if err != nil {
		t.Fatal(err)
	}
	root, err := peerv2.DeriveRootKey(shared, fullBinding)
	zeroV2Bytes(shared)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroV2Bytes(root)
	sequencer := peerv2.NewSequencer(4096)
	confirmMAC, err := peerv2.LinkConfirmationMAC(root, fullBinding)
	if err != nil {
		t.Fatal(err)
	}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_LINK_CONFIRM, v2ControlChannelID, v2ControlStreamID, &remotev2.LinkConfirm{LinkId: linkID, TranscriptMac: confirmMAC})
	readyRecord, readyPlaintext := controller.nextRecord(root, sequencer)
	if readyRecord.GetFrameType() != remotev2.FrameType_FRAME_TYPE_LINK_READY {
		t.Fatalf("first encrypted record type = %v, want LINK_READY", readyRecord.GetFrameType())
	}
	linkReady := new(remotev2.LinkReady)
	if err := proto.Unmarshal(readyPlaintext, linkReady); err != nil || linkReady.GetLinkId() != linkID || linkReady.GetActiveKeyId() != 1 {
		t.Fatalf("LinkReady = %#v, %v", linkReady, err)
	}
	unknownLinkID := uuid.NewString()
	controller.send(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Link{Link: &remotev2.LinkEnvelope{
		LinkId: unknownLinkID,
		Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
			LinkId: unknownLinkID, ChannelId: uuid.NewString(), StreamId: uuid.NewString(),
			FrameType: remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, StreamSequence: 1,
		}},
	}}})
	unknownLinkRejection := controller.next().GetStreamRejected()
	if unknownLinkRejection == nil || unknownLinkRejection.GetLinkId() != unknownLinkID ||
		unknownLinkRejection.GetChannelId() != "" || unknownLinkRejection.GetStreamId() != "" ||
		unknownLinkRejection.GetReason() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_NOT_FOUND {
		t.Fatalf("unknown Link rejection = %#v; want Link-scoped STREAM_NOT_FOUND", unknownLinkRejection)
	}

	queryChannelID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, queryChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: queryChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY, Scopes: []string{"remote.peer.query"},
	})
	queryAccepted := controller.expectChannelAccept(root, sequencer, queryChannelID)
	if len(queryAccepted.GetGrantedScopes()) != 1 || queryAccepted.GetGrantedScopes()[0] != "remote.peer.query" {
		t.Fatalf("device query Channel accepted scopes = %#v", queryAccepted.GetGrantedScopes())
	}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, queryChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: queryChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY, Scopes: []string{"remote.peer.query"},
	})
	if replayed := controller.expectChannelAccept(root, sequencer, queryChannelID); len(replayed.GetGrantedScopes()) != 1 || replayed.GetGrantedScopes()[0] != "remote.peer.query" {
		t.Fatalf("idempotent device query Channel acceptance = %#v", replayed)
	}
	queryStreamID := uuid.NewString()
	queryOperationID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, queryChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: queryChannelID, StreamId: queryStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: queryOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, queryChannelID, queryStreamID, &remotev2.RpcRequest{
		OperationId: queryOperationID, AttemptId: uuid.NewString(), Method: "agent.capabilities.get", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)), Payload: []byte(`{}`),
	})
	queryResponse := controller.expectRPCResponse(root, sequencer, queryStreamID, queryOperationID)
	if queryResponse.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED || queryResponse.GetSafeErrorCode() != "" {
		t.Fatalf("device query RPC response = %#v", queryResponse)
	}

	// Exercise the reported project-management failure through the production
	// client -> Relay -> Device Link. Three consecutive device-scoped creates
	// must share the same Link, and the newest project must be immediately
	// removable with the revision returned by project.create.
	type projectMutation struct {
		ID       uuid.UUID `json:"id"`
		Revision uint64    `json:"revision"`
	}
	createdProjects := make([]projectMutation, 0, 3)
	createdRoots := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		rootPath := t.TempDir()
		handle, issueErr := projectDirectoryBrowserFor(state).issue(rootPath, time.Now().UTC())
		if issueErr != nil {
			t.Fatal(issueErr)
		}
		payload, marshalErr := json.Marshal(map[string]any{
			"name": "relay-crud-" + strconv.Itoa(index+1), "directoryId": handle.ID,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		response := controller.callRPC(root, sequencer, queryChannelID, "project.create", payload)
		if response.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED || response.GetSafeErrorCode() != "" {
			t.Fatalf("project.create #%d response = %#v", index+1, response)
		}
		var created projectMutation
		if err := json.Unmarshal(response.GetPayload(), &created); err != nil || created.ID == uuid.Nil || created.Revision == 0 {
			t.Fatalf("project.create #%d payload = %s, %v", index+1, response.GetPayload(), err)
		}
		for _, previous := range createdProjects {
			if previous.ID == created.ID {
				t.Fatalf("project.create #%d reused active ID %s", index+1, created.ID)
			}
		}
		createdProjects = append(createdProjects, created)
		createdRoots = append(createdRoots, rootPath)
	}
	removePayload, err := json.Marshal(map[string]any{
		"projectId": createdProjects[2].ID, "expectedRevision": createdProjects[2].Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	removeResponse := controller.callRPC(root, sequencer, queryChannelID, "project.remove", removePayload)
	if removeResponse.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED || removeResponse.GetSafeErrorCode() != "" {
		t.Fatalf("immediate project.remove response = %#v", removeResponse)
	}
	var removed struct {
		Removed   bool      `json:"removed"`
		ProjectID uuid.UUID `json:"projectId"`
		Revision  uint64    `json:"revision"`
	}
	if err := json.Unmarshal(removeResponse.GetPayload(), &removed); err != nil || !removed.Removed || removed.ProjectID != createdProjects[2].ID || removed.Revision <= createdProjects[2].Revision {
		t.Fatalf("project.remove payload = %s, %v", removeResponse.GetPayload(), err)
	}
	if info, err := os.Stat(createdRoots[2]); err != nil || !info.IsDir() {
		t.Fatalf("project.remove changed the device directory: %v, %v", info, err)
	}

	projectChannelID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, projectChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: projectChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_PROJECT, ProjectId: project.ID.String(), Scopes: []string{"remote.peer.ai.chat"},
	})
	projectAccepted := controller.expectChannelAccept(root, sequencer, projectChannelID)
	if len(projectAccepted.GetGrantedScopes()) != 1 || projectAccepted.GetGrantedScopes()[0] != "remote.peer.ai.chat" {
		t.Fatalf("project AI Channel accepted scopes = %#v", projectAccepted.GetGrantedScopes())
	}
	projectStreamID := uuid.NewString()
	projectOperationID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, projectChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: projectChannelID, StreamId: projectStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: projectOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, projectChannelID, projectStreamID, &remotev2.RpcRequest{
		OperationId: projectOperationID, AttemptId: uuid.NewString(), Method: "conversation.list", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)), Payload: []byte(`{}`),
	})
	projectResponse := controller.expectRPCResponse(root, sequencer, projectStreamID, projectOperationID)
	if projectResponse.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED || projectResponse.GetSafeErrorCode() != "" {
		t.Fatalf("project AI RPC response = %#v", projectResponse)
	}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_CLOSE, projectChannelID, v2ChannelControlStreamID, &remotev2.StreamClose{
		ChannelId: projectChannelID, StreamId: projectStreamID, Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED,
	})

	// Task reads use their v2 project Channel's remote.peer.task.control
	// authorization even though the underlying task.list method has a narrower
	// read-only dispatcher alias. This catches a scope bridge regression before
	// task UI code reaches a real Device.
	taskChannelID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, taskChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: taskChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_PROJECT, ProjectId: project.ID.String(), Scopes: []string{"remote.peer.task.control"},
	})
	controller.expectChannelAccept(root, sequencer, taskChannelID)
	taskStreamID, taskOperationID := uuid.NewString(), uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, taskChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: taskChannelID, StreamId: taskStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: taskOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, taskChannelID, taskStreamID, &remotev2.RpcRequest{
		OperationId: taskOperationID, AttemptId: uuid.NewString(), Method: "task.list", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)), Payload: []byte(`{}`),
	})
	if response := controller.expectRPCResponse(root, sequencer, taskStreamID, taskOperationID); response.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("project Task RPC response = %#v", response)
	}

	// Terminal command execution is also carried as an encrypted, project-bound
	// RPC Stream. Use pwd, which the Device's restricted terminal contract
	// permits on every supported host, without exposing its path to the Relay.
	terminalChannelID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, terminalChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: terminalChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_PROJECT, ProjectId: project.ID.String(), Scopes: []string{"remote.peer.terminal"},
	})
	controller.expectChannelAccept(root, sequencer, terminalChannelID)
	terminalStreamID, terminalOperationID := uuid.NewString(), uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, terminalChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: terminalChannelID, StreamId: terminalStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: terminalOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, terminalChannelID, terminalStreamID, &remotev2.RpcRequest{
		OperationId: terminalOperationID, AttemptId: uuid.NewString(), Method: "terminal.execute", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)), Payload: []byte(`{"command":"pwd"}`),
	})
	if response := controller.expectRPCResponse(root, sequencer, terminalStreamID, terminalOperationID); response.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("project Terminal RPC response = %#v", response)
	}

	// The interactive terminal uses the same project-bound encrypted RPC path,
	// but terminal.attach must remain a live Stream. A one-shot dispatcher would
	// return an empty response before this output is emitted, leaving the desktop
	// xterm permanently blank and causing a tight attach loop.
	interactiveTerminalChannelID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, interactiveTerminalChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: interactiveTerminalChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_PROJECT, ProjectId: project.ID.String(), Scopes: []string{"remote.peer.terminal.interactive"},
	})
	controller.expectChannelAccept(root, sequencer, interactiveTerminalChannelID)
	terminalOpenStreamID, terminalOpenOperationID := uuid.NewString(), uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, interactiveTerminalChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: interactiveTerminalChannelID, StreamId: terminalOpenStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: terminalOpenOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, interactiveTerminalChannelID, terminalOpenStreamID, &remotev2.RpcRequest{
		OperationId: terminalOpenOperationID, AttemptId: uuid.NewString(), Method: "terminal.open", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload: []byte(`{"clientRequestId":"` + uuid.NewString() + `","cwd":"","rows":24,"columns":80}`),
	})
	terminalOpenResponse := controller.expectRPCResponse(root, sequencer, terminalOpenStreamID, terminalOpenOperationID)
	if terminalOpenResponse.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED || terminalOpenResponse.GetSafeErrorCode() != "" {
		t.Fatalf("interactive terminal.open response = %#v", terminalOpenResponse)
	}
	var terminalOpened struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(terminalOpenResponse.GetPayload(), &terminalOpened); err != nil || uuid.Validate(terminalOpened.SessionID) != nil {
		t.Fatalf("interactive terminal.open payload = %s, %v", terminalOpenResponse.GetPayload(), err)
	}
	terminalProcess := terminalStarter.latest()
	if terminalProcess == nil {
		t.Fatal("interactive terminal did not start its supervised PTY")
	}

	terminalAttachStreamID, terminalAttachOperationID := uuid.NewString(), uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, interactiveTerminalChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: interactiveTerminalChannelID, StreamId: terminalAttachStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: terminalAttachOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, interactiveTerminalChannelID, terminalAttachStreamID, &remotev2.RpcRequest{
		OperationId: terminalAttachOperationID, AttemptId: uuid.NewString(), Method: "terminal.attach", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload: []byte(`{"sessionId":"` + terminalOpened.SessionID + `","lastSequence":0,"waitSeconds":1}`),
	})
	terminalOutput := []byte("remote-v2-terminal-output\r\n")
	if err := terminalProcess.emit(terminalOutput); err != nil {
		t.Fatal(err)
	}
	terminalEvent := controller.expectRPCEvent(root, sequencer, terminalAttachStreamID, terminalAttachOperationID)
	var terminalEventPayload struct {
		SessionID string `json:"sessionId"`
		Type      string `json:"type"`
		Sequence  uint64 `json:"sequence"`
		Encoding  string `json:"encoding"`
		Data      string `json:"data"`
	}
	if err := json.Unmarshal(terminalEvent.GetPayload(), &terminalEventPayload); err != nil {
		t.Fatalf("interactive terminal event payload = %s, %v", terminalEvent.GetPayload(), err)
	}
	decodedTerminalOutput, decodeErr := base64.RawURLEncoding.Strict().DecodeString(terminalEventPayload.Data)
	if decodeErr != nil || terminalEventPayload.SessionID != terminalOpened.SessionID || terminalEventPayload.Type != "output" || terminalEventPayload.Sequence != 1 ||
		terminalEventPayload.Encoding != "base64url" || !bytes.Equal(decodedTerminalOutput, terminalOutput) {
		t.Fatalf("interactive terminal event = %#v, decoded=%q, err=%v", terminalEventPayload, decodedTerminalOutput, decodeErr)
	}
	terminalAttachResponse := controller.expectRPCResponse(root, sequencer, terminalAttachStreamID, terminalAttachOperationID)
	if terminalAttachResponse.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("interactive terminal.attach response = %#v", terminalAttachResponse)
	}

	// terminal v4 keeps one full-duplex binary Stream open. Output is delivered
	// as raw bytes, while cumulative input ACK and byte credit allow the client
	// to pipeline batches without one RPC round-trip per key.
	terminalDuplexStreamID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, interactiveTerminalChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: interactiveTerminalChannelID, StreamId: terminalDuplexStreamID, Kind: remotev2.StreamKind_STREAM_KIND_TERMINAL, OperationId: terminalOpened.SessionID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_DATA, interactiveTerminalChannelID, terminalDuplexStreamID, &remotev2.TerminalStreamFrame{
		SessionId: terminalOpened.SessionID,
		Body: &remotev2.TerminalStreamFrame_Hello{Hello: &remotev2.TerminalStreamHello{
			AfterOutputSequence: 1, OutputCreditBytes: 64 << 10,
		}},
	})
	if frame := controller.expectTerminalFrame(root, sequencer, terminalDuplexStreamID, terminalOpened.SessionID); frame.GetInputAck() == nil || frame.GetInputAck().GetThroughSequence() != 0 {
		t.Fatalf("terminal v4 initial input ACK = %#v", frame)
	}
	if frame := controller.expectTerminalFrame(root, sequencer, terminalDuplexStreamID, terminalOpened.SessionID); frame.GetResizeAck() == nil || frame.GetResizeAck().GetThroughSequence() != 0 {
		t.Fatalf("terminal v4 initial resize ACK = %#v", frame)
	}
	if frame := controller.expectTerminalFrame(root, sequencer, terminalDuplexStreamID, terminalOpened.SessionID); frame.GetWindowUpdate() == nil || frame.GetWindowUpdate().GetCreditBytes() != 32<<10 {
		t.Fatalf("terminal v4 initial input credit = %#v", frame)
	}
	// A zero-watermark, zero-credit OutputAck is the content-free v4 keepalive.
	// The following output proves it neither closes the Stream nor changes the
	// output window before normal traffic resumes.
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_DATA, interactiveTerminalChannelID, terminalDuplexStreamID, &remotev2.TerminalStreamFrame{
		SessionId: terminalOpened.SessionID,
		Body:      &remotev2.TerminalStreamFrame_OutputAck{OutputAck: &remotev2.TerminalOutputAck{}},
	})
	terminalV4Output := []byte("raw-terminal-v4-输出\r\n")
	if err := terminalProcess.emit(terminalV4Output); err != nil {
		t.Fatal(err)
	}
	v4OutputFrame := controller.expectTerminalFrame(root, sequencer, terminalDuplexStreamID, terminalOpened.SessionID)
	if v4OutputFrame.GetOutput() == nil || v4OutputFrame.GetOutput().GetSequence() != 2 || !bytes.Equal(v4OutputFrame.GetOutput().GetData(), terminalV4Output) {
		t.Fatalf("terminal v4 output = %#v", v4OutputFrame)
	}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_DATA, interactiveTerminalChannelID, terminalDuplexStreamID, &remotev2.TerminalStreamFrame{
		SessionId: terminalOpened.SessionID,
		Body: &remotev2.TerminalStreamFrame_OutputAck{OutputAck: &remotev2.TerminalOutputAck{
			ThroughSequence: 2, CreditBytes: uint32(len(terminalV4Output)),
		}},
	})
	terminalV4Input := []byte("echo terminal-v4\r")
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_DATA, interactiveTerminalChannelID, terminalDuplexStreamID, &remotev2.TerminalStreamFrame{
		SessionId: terminalOpened.SessionID,
		Body:      &remotev2.TerminalStreamFrame_Input{Input: &remotev2.TerminalInput{Sequence: 1, Data: terminalV4Input}},
	})
	if frame := controller.expectTerminalFrame(root, sequencer, terminalDuplexStreamID, terminalOpened.SessionID); frame.GetInputAck() == nil || frame.GetInputAck().GetThroughSequence() != 1 {
		t.Fatalf("terminal v4 input ACK = %#v", frame)
	}
	if frame := controller.expectTerminalFrame(root, sequencer, terminalDuplexStreamID, terminalOpened.SessionID); frame.GetWindowUpdate() == nil || frame.GetWindowUpdate().GetCreditBytes() != uint32(len(terminalV4Input)) {
		t.Fatalf("terminal v4 replenished credit = %#v", frame)
	}
	writes := terminalProcess.snapshotWrites()
	if len(writes) == 0 || !bytes.Equal(writes[len(writes)-1], terminalV4Input) {
		t.Fatalf("terminal v4 PTY writes = %#v", writes)
	}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_DATA, interactiveTerminalChannelID, terminalDuplexStreamID, &remotev2.TerminalStreamFrame{
		SessionId: terminalOpened.SessionID,
		Body:      &remotev2.TerminalStreamFrame_Close{Close: &remotev2.TerminalClose{Reason: "client_close"}},
	})
	if frame := controller.expectTerminalFrame(root, sequencer, terminalDuplexStreamID, terminalOpened.SessionID); frame.GetExit() == nil || frame.GetExit().GetSequence() != 3 {
		t.Fatalf("terminal v4 exit = %#v", frame)
	}

	// Attach to a genuinely active generation before rekeying.  The initial
	// cursor suppresses the durable bootstrap range, leaving this Stream waiting
	// for the next AI delta instead of treating a completed one-shot RPC as an
	// AI stream.
	aiStreamID, aiAttachOperationID := uuid.NewString(), uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, projectChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: projectChannelID, StreamId: aiStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: aiAttachOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, projectChannelID, aiStreamID, &remotev2.RpcRequest{
		OperationId: aiAttachOperationID, AttemptId: uuid.NewString(), Method: "conversation.generation.attach", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload: []byte(`{"conversationId":"` + aiConversation.ID + `","generationId":"` + aiTurn.GenerationID + `","afterSequence":` + strconv.FormatUint(aiHighWatermark, 10) + `}`),
	})

	// A dedicated Event Stream is similarly left live across the rekey. Its
	// cursor is deliberately non-zero: zero requests the authoritative reset
	// snapshot and therefore is not a resumable subscription.
	eventChannelID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, eventChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: eventChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_PROJECT, ProjectId: project.ID.String(), Scopes: []string{"remote.peer.events"},
	})
	controller.expectChannelAccept(root, sequencer, eventChannelID)
	eventStreamID, eventSubscriptionID := uuid.NewString(), uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, eventChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: eventChannelID, StreamId: eventStreamID, Kind: remotev2.StreamKind_STREAM_KIND_EVENT, OperationId: eventSubscriptionID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_EVENT_SUBSCRIBE, eventChannelID, eventStreamID, &remotev2.EventSubscribe{
		SubscriptionId: eventSubscriptionID, AfterSequence: initialProjectEvent.Sequence,
	})
	eventReady := controller.expectRPCEvent(root, sequencer, eventStreamID, eventSubscriptionID)
	if eventReady.GetEventSequence() != 0 || eventReady.GetHighWatermark() != initialProjectEvent.Sequence {
		t.Fatalf("Event subscription control = %#v", eventReady)
	}

	link := links.get(linkID)
	if link == nil {
		t.Fatal("Agent did not retain the active v2 Link")
	}
	link.mu.Lock()
	queryChannel, projectChannel := link.channels[queryChannelID], link.channels[projectChannelID]
	link.mu.Unlock()
	if queryChannel == nil || queryChannel.kind != remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY || queryChannel.projectID != "" {
		t.Fatalf("Agent query Channel binding = %#v", queryChannel)
	}
	if projectChannel == nil || projectChannel.kind != remotev2.ChannelKind_CHANNEL_KIND_PROJECT || projectChannel.projectID != project.ID.String() {
		t.Fatalf("Agent project Channel binding = %#v", projectChannel)
	}

	// A download uses one small preparation RPC, then only the dedicated bulk
	// Stream. Repeating the manifest before ACK simulates a Carrier recovery:
	// Device retransmits exactly the unacknowledged first chunk rather than
	// recreating a transfer or falling back to JSON RPC chunks.
	downloadChannelID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, downloadChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: downloadChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_PROJECT, ProjectId: project.ID.String(), Scopes: []string{"remote.peer.file.receive"},
	})
	controller.expectChannelAccept(root, sequencer, downloadChannelID)
	downloadTransferID := uuid.NewString()
	downloadPrepareStreamID, downloadPrepareOperationID := uuid.NewString(), uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, downloadChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: downloadChannelID, StreamId: downloadPrepareStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: downloadPrepareOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, downloadChannelID, downloadPrepareStreamID, &remotev2.RpcRequest{
		OperationId: downloadPrepareOperationID, AttemptId: uuid.NewString(), Method: "file.download.prepare", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload: []byte(`{"transferId":"` + downloadTransferID + `","path":"v2-download.bin"}`),
	})
	if response := controller.expectRPCResponse(root, sequencer, downloadPrepareStreamID, downloadPrepareOperationID); response.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("download preparation response = %#v", response)
	}
	downloadStreamID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, downloadChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: downloadChannelID, StreamId: downloadStreamID, Kind: remotev2.StreamKind_STREAM_KIND_FILE, OperationId: downloadTransferID,
	})
	downloadDigest := sha256.Sum256(downloadPayload)
	downloadManifest := &remotev2.FileManifest{TransferId: downloadTransferID, TotalLength: uint64(len(downloadPayload)), ChunkSize: v2MaximumFileChunkSize, Sha256: downloadDigest[:], RelativePathHandle: downloadTransferID}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_MANIFEST, downloadChannelID, downloadStreamID, downloadManifest)
	firstDownloadChunk := controller.expectFileChunk(root, sequencer, downloadStreamID, downloadTransferID, 0)
	// Carrier delivery is ambiguous after WebSocket.send succeeds. Reasserting
	// an active Stream with identical metadata is idempotent and must not reset
	// its file cursor; changed or tombstoned Stream IDs remain prohibited.
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, downloadChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: downloadChannelID, StreamId: downloadStreamID, Kind: remotev2.StreamKind_STREAM_KIND_FILE, OperationId: downloadTransferID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_MANIFEST, downloadChannelID, downloadStreamID, downloadManifest)
	replayedFirstDownloadChunk := controller.expectFileChunk(root, sequencer, downloadStreamID, downloadTransferID, 0)
	if !bytes.Equal(firstDownloadChunk.GetPayload(), replayedFirstDownloadChunk.GetPayload()) || !bytes.Equal(firstDownloadChunk.GetChunkHash(), replayedFirstDownloadChunk.GetChunkHash()) {
		t.Fatal("replayed bulk download chunk differs from the original")
	}
	// Keep the download Stream open but paused on its first ACK, then force a
	// complete v2 rekey. The remaining 99 MiB must continue on key generation
	// two without losing or reordering chunks; key generation one remains
	// available for the delayed/replayed first chunk during its grace period.
	// The AI generation and Event subscription are both actively waiting on
	// their original Streams at the same time.
	nextRoot := forceV2E2ERekey(t, controller, root, sequencer, clientPrivate, state.identity.Public().(ed25519.PublicKey), []peerv2.StreamBoundary{
		{StreamID: aiStreamID, NextSequence: 1},
		{StreamID: downloadStreamID, NextSequence: 1},
		{StreamID: eventStreamID, NextSequence: 1},
	})
	defer zeroV2Bytes(nextRoot)
	root = nextRoot

	// Publish a committed AI delta after the rekey. It advances both the
	// generation's direct Stream and the project's durable Event subscription;
	// delivery may race, so assert both encrypted records without assuming an
	// order. Neither stream is recreated.
	_, aiDelta, err := state.business.appendAIConversationTextDelta(ctx, project.ID, aiConversation.ID, aiTurn.GenerationID, aiTurn.Assistant.ID, "rekeyed delta", time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	projectEventInfo, err := state.business.agentEventStreamInfo(ctx, project.ID)
	if err != nil || projectEventInfo.HighWatermark <= initialProjectEvent.Sequence {
		t.Fatalf("project Event watermark after AI delta = %#v, %v", projectEventInfo, err)
	}
	gotAIDelta, gotProjectEvent := false, false
	for !gotAIDelta || !gotProjectEvent {
		record, plaintext := controller.nextRecord(root, sequencer)
		event := new(remotev2.RpcEvent)
		if record.GetFrameType() != remotev2.FrameType_FRAME_TYPE_RPC_EVENT || proto.Unmarshal(plaintext, event) != nil {
			zeroV2Bytes(plaintext)
			t.Fatalf("rekeyed active Stream record = %#v", record)
		}
		zeroV2Bytes(plaintext)
		switch record.GetStreamId() {
		case aiStreamID:
			if event.GetOperationId() != aiAttachOperationID || event.GetEventSequence() != aiDelta.Sequence || event.GetHighWatermark() != aiDelta.Sequence {
				t.Fatalf("rekeyed AI event = %#v", event)
			}
			gotAIDelta = true
		case eventStreamID:
			if event.GetEventSequence() == 0 {
				// A content-free subscription heartbeat may race the committed
				// project event after a slower rekey/test host. It does not advance
				// the durable cursor and is not the event asserted below.
				continue
			}
			if event.GetOperationId() != eventSubscriptionID || event.GetEventSequence() != projectEventInfo.HighWatermark || event.GetHighWatermark() != projectEventInfo.HighWatermark {
				t.Fatalf("rekeyed project Event = %#v", event)
			}
			gotProjectEvent = true
		default:
			t.Fatalf("unexpected rekeyed active Stream %q", record.GetStreamId())
		}
	}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_EVENT_ACK, eventChannelID, eventStreamID, &remotev2.EventAck{
		SubscriptionId: eventSubscriptionID, HighWatermark: projectEventInfo.HighWatermark,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_CLOSE, eventChannelID, v2ChannelControlStreamID, &remotev2.StreamClose{
		ChannelId: eventChannelID, StreamId: eventStreamID, Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED,
	})

	downloadChunkBytes := int(downloadManifest.GetChunkSize())
	for index, offset := uint64(0), 0; offset < len(downloadPayload); index++ {
		chunk := replayedFirstDownloadChunk
		if index != 0 {
			chunk = controller.expectFileChunk(root, sequencer, downloadStreamID, downloadTransferID, index)
		}
		end := min(offset+downloadChunkBytes, len(downloadPayload))
		if !bytes.Equal(chunk.GetPayload(), downloadPayload[offset:end]) || sha256.Sum256(chunk.GetPayload()) != bytesToSHA256(chunk.GetChunkHash()) {
			t.Fatalf("download bulk chunk %d failed integrity validation", index)
		}
		controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_ACK, downloadChannelID, downloadStreamID, &remotev2.FileAck{TransferId: downloadTransferID, ConfirmedIndexes: []uint64{index}})
		if index == 0 {
			if _, err := links.get(linkID).keys.RootKey(1); err != nil {
				t.Fatalf("Agent did not retain the prior key generation during rekey grace: %v", err)
			}
		}
		offset = end
	}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_COMMIT, downloadChannelID, downloadStreamID, &remotev2.FileCommit{TransferId: downloadTransferID, Sha256: downloadDigest[:]})
	controller.expectFileAck(root, sequencer, downloadStreamID, downloadTransferID)
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_CLOSE, projectChannelID, v2ChannelControlStreamID, &remotev2.StreamClose{
		ChannelId: projectChannelID, StreamId: aiStreamID, Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_CANCELLED,
	})
	eventually(t, time.Second, func() bool {
		link.mu.Lock()
		defer link.mu.Unlock()
		return link.channels[projectChannelID] != nil && link.channels[projectChannelID].streams[aiStreamID] == nil
	})

	// The reciprocal direction verifies persisted upload ACKs, duplicate
	// manifest/chunk recovery and an idempotent commit acknowledgement.
	uploadChannelID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, uploadChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: uploadChannelID, Kind: remotev2.ChannelKind_CHANNEL_KIND_PROJECT, ProjectId: project.ID.String(), Scopes: []string{"remote.peer.file.send"},
	})
	controller.expectChannelAccept(root, sequencer, uploadChannelID)
	uploadPayload := bytes.Repeat([]byte("remote/v2 upload bulk payload\n"), 3000)
	uploadDigest := sha256.Sum256(uploadPayload)
	uploadTransferID := uuid.NewString()
	uploadPrepareStreamID, uploadPrepareOperationID := uuid.NewString(), uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, uploadChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: uploadChannelID, StreamId: uploadPrepareStreamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: uploadPrepareOperationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, uploadChannelID, uploadPrepareStreamID, &remotev2.RpcRequest{
		OperationId: uploadPrepareOperationID, AttemptId: uuid.NewString(), Method: "file.upload.prepare", Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload: []byte(`{"transferId":"` + uploadTransferID + `","path":"v2-upload.bin","size":` + strconv.Itoa(len(uploadPayload)) + `,"sha256":"` + base64.RawURLEncoding.EncodeToString(uploadDigest[:]) + `"}`),
	})
	if response := controller.expectRPCResponse(root, sequencer, uploadPrepareStreamID, uploadPrepareOperationID); response.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("upload preparation response = %#v", response)
	}
	uploadStreamID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, uploadChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: uploadChannelID, StreamId: uploadStreamID, Kind: remotev2.StreamKind_STREAM_KIND_FILE, OperationId: uploadTransferID,
	})
	uploadManifest := &remotev2.FileManifest{TransferId: uploadTransferID, TotalLength: uint64(len(uploadPayload)), ChunkSize: fileChunkBytes, Sha256: uploadDigest[:], RelativePathHandle: uploadTransferID}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_MANIFEST, uploadChannelID, uploadStreamID, uploadManifest)
	controller.expectFileAck(root, sequencer, uploadStreamID, uploadTransferID)
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, uploadChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: uploadChannelID, StreamId: uploadStreamID, Kind: remotev2.StreamKind_STREAM_KIND_FILE, OperationId: uploadTransferID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_MANIFEST, uploadChannelID, uploadStreamID, uploadManifest)
	controller.expectFileAck(root, sequencer, uploadStreamID, uploadTransferID)
	for index, offset := uint64(0), 0; offset < len(uploadPayload); index++ {
		end := min(offset+fileChunkBytes, len(uploadPayload))
		payload := uploadPayload[offset:end]
		chunkDigest := sha256.Sum256(payload)
		controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_CHUNK, uploadChannelID, uploadStreamID, &remotev2.FileChunk{
			TransferId: uploadTransferID, Index: index, ChunkHash: chunkDigest[:], Payload: payload,
		})
		ack := controller.expectFileAck(root, sequencer, uploadStreamID, uploadTransferID)
		if len(ack.GetConfirmedIndexes()) != 1 || ack.GetConfirmedIndexes()[0] != index {
			t.Fatalf("upload bulk chunk %d acknowledgement = %#v", index, ack)
		}
		offset = end
	}
	uploadCommit := &remotev2.FileCommit{TransferId: uploadTransferID, Sha256: uploadDigest[:]}
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_COMMIT, uploadChannelID, uploadStreamID, uploadCommit)
	controller.expectFileAck(root, sequencer, uploadStreamID, uploadTransferID)
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_FILE_COMMIT, uploadChannelID, uploadStreamID, uploadCommit)
	controller.expectFileAck(root, sequencer, uploadStreamID, uploadTransferID)
	storedUpload, err := os.ReadFile(filepath.Join(workspace, "v2-upload.bin"))
	if err != nil || !bytes.Equal(storedUpload, uploadPayload) {
		t.Fatalf("bulk upload persisted content = %d bytes, %v", len(storedUpload), err)
	}
}

type v2E2EDeviceAuthenticator struct {
	deviceID string
	userID   string
	epoch    uint64
}

func (auth v2E2EDeviceAuthenticator) AuthenticateV2Device(_ context.Context, _ *remotev2.CarrierEnvelope, hello *remotev2.CarrierHello, _ time.Time) (relayserver.V2DevicePrincipal, error) {
	if hello == nil || hello.GetDeviceId() != auth.deviceID || hello.GetDeviceConnectionEpoch() != auth.epoch {
		return relayserver.V2DevicePrincipal{}, errors.New("test device carrier identity mismatch")
	}
	return relayserver.V2DevicePrincipal{
		DeviceID: auth.deviceID, UserID: auth.userID, ConnectionEpoch: auth.epoch,
		AssignmentVersion: 1, GrantVersion: 1,
	}, nil
}

type v2E2EController struct {
	t         *testing.T
	ctx       context.Context
	socket    *websocket.Conn
	carrierID string
	epoch     uint64
	linkID    string
	keyID     uint64
	nextOut   uint64
	lastIn    uint64
}

func (controller *v2E2EController) send(envelope *remotev2.CarrierEnvelope) {
	controller.t.Helper()
	if envelope == nil {
		controller.t.Fatal("cannot write a nil v2 Carrier envelope")
	}
	controller.nextOut++
	envelope.ProtocolMajor = 2
	envelope.CarrierId = controller.carrierID
	envelope.CarrierEpoch = controller.epoch
	envelope.PacketSequence = controller.nextOut
	envelope.AcknowledgedSequence = controller.lastIn
	payload, err := proto.Marshal(envelope)
	if err != nil {
		controller.t.Fatal(err)
	}
	if err := controller.socket.Write(controller.ctx, websocket.MessageBinary, payload); err != nil {
		controller.t.Fatal(err)
	}
}

func (controller *v2E2EController) next() *remotev2.CarrierEnvelope {
	controller.t.Helper()
	for {
		messageType, payload, err := controller.socket.Read(controller.ctx)
		if err != nil {
			controller.t.Fatal(err)
		}
		if messageType != websocket.MessageBinary {
			controller.t.Fatalf("v2 Carrier message type = %v", messageType)
		}
		envelope := new(remotev2.CarrierEnvelope)
		if err := proto.Unmarshal(payload, envelope); err != nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
			controller.t.Fatalf("decode v2 Carrier envelope: %v", err)
		}
		if envelope.GetProtocolMajor() != 2 || envelope.GetCarrierId() != controller.carrierID || envelope.GetCarrierEpoch() != controller.epoch || envelope.GetPacketSequence() != controller.lastIn+1 {
			controller.t.Fatalf("v2 Carrier inbound sequencing = %#v", envelope)
		}
		controller.lastIn = envelope.GetPacketSequence()
		if ping := envelope.GetPing(); ping != nil {
			controller.send(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Pong{Pong: &remotev2.CarrierPong{MonotonicMillis: ping.GetMonotonicMillis()}}})
			continue
		}
		return envelope
	}
}

func (controller *v2E2EController) sendRecord(root []byte, sequencer *peerv2.Sequencer, frameType remotev2.FrameType, channelID, streamID string, message proto.Message) {
	controller.sendRecordWithKey(root, controller.keyID, sequencer, frameType, channelID, streamID, message)
}

func (controller *v2E2EController) sendRecordWithKey(root []byte, keyID uint64, sequencer *peerv2.Sequencer, frameType remotev2.FrameType, channelID, streamID string, message proto.Message) {
	controller.t.Helper()
	plaintext, err := proto.Marshal(message)
	if err != nil {
		controller.t.Fatal(err)
	}
	sequence, err := sequencer.Next(keyID, peerv2.DirectionClientToDevice, streamID)
	if err != nil {
		controller.t.Fatal(err)
	}
	metadata := peerv2.RecordMetadata{
		LinkID: controller.linkID, ChannelID: channelID, StreamID: streamID, KeyID: keyID,
		Direction: peerv2.DirectionClientToDevice, FrameType: peerv2.FrameType(frameType), StreamSequence: sequence,
	}
	key, err := v2AgentRecordKey(root, metadata)
	if err != nil {
		controller.t.Fatal(err)
	}
	ciphertext, err := peerv2.Seal(key, plaintext, metadata)
	zeroV2Bytes(key)
	if err != nil {
		controller.t.Fatal(err)
	}
	controller.send(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Link{Link: &remotev2.LinkEnvelope{
		LinkId: metadata.LinkID,
		Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
			LinkId: metadata.LinkID, ChannelId: channelID, StreamId: streamID, KeyId: keyID,
			Direction: remotev2.Direction_DIRECTION_CLIENT_TO_DEVICE, FrameType: frameType, StreamSequence: sequence, Ciphertext: ciphertext,
		}},
	}}})
}

func (controller *v2E2EController) nextRecord(root []byte, sequencer *peerv2.Sequencer) (*remotev2.EncryptedRecord, []byte) {
	controller.t.Helper()
	for {
		envelope := controller.next()
		link := envelope.GetLink()
		record := link.GetEncrypted()
		if link == nil || record == nil {
			controller.t.Fatalf("expected encrypted Link record, got %#v", envelope)
		}
		metadata := peerv2.RecordMetadata{
			LinkID: record.GetLinkId(), ChannelID: record.GetChannelId(), StreamID: record.GetStreamId(), KeyID: record.GetKeyId(),
			Direction: peerv2.DirectionDeviceToClient, FrameType: peerv2.FrameType(record.GetFrameType()), StreamSequence: record.GetStreamSequence(),
		}
		key, err := v2AgentRecordKey(root, metadata)
		if err != nil {
			controller.t.Fatal(err)
		}
		plaintext, err := peerv2.Open(key, record.GetCiphertext(), metadata)
		zeroV2Bytes(key)
		if err != nil {
			controller.t.Fatal(err)
		}
		if err := sequencer.AcceptInbound(metadata); err != nil {
			zeroV2Bytes(plaintext)
			controller.t.Fatal(err)
		}
		// Normal RPC completion releases only Device-side state; it does not send a
		// control close. An Agent-originated close therefore reports failStream and
		// must not be hidden as a later read timeout.
		if record.GetFrameType() == remotev2.FrameType_FRAME_TYPE_STREAM_CLOSE &&
			record.GetStreamId() == v2ChannelControlStreamID {
			closed := new(remotev2.StreamClose)
			if proto.Unmarshal(plaintext, closed) != nil {
				zeroV2Bytes(plaintext)
				controller.t.Fatalf("invalid Agent StreamClose record = %#v", record)
			}
			zeroV2Bytes(plaintext)
			controller.t.Fatalf("unexpected Agent StreamClose = %#v", closed)
		}
		return record, plaintext
	}
}

func (controller *v2E2EController) expectChannelAccept(root []byte, sequencer *peerv2.Sequencer, channelID string) *remotev2.ChannelAccept {
	controller.t.Helper()
	record, plaintext := controller.nextRecord(root, sequencer)
	defer zeroV2Bytes(plaintext)
	accepted := new(remotev2.ChannelAccept)
	if record.GetFrameType() != remotev2.FrameType_FRAME_TYPE_CHANNEL_ACCEPT || record.GetChannelId() != channelID || record.GetStreamId() != v2ChannelControlStreamID || proto.Unmarshal(plaintext, accepted) != nil || accepted.GetChannelId() != channelID {
		controller.t.Fatalf("ChannelAccept record = %#v", record)
	}
	return accepted
}

func (controller *v2E2EController) expectRPCResponse(root []byte, sequencer *peerv2.Sequencer, streamID, operationID string) *remotev2.RpcResponse {
	controller.t.Helper()
	for {
		record, plaintext := controller.nextRecord(root, sequencer)
		if record.GetStreamId() != streamID {
			zeroV2Bytes(plaintext)
			controller.t.Fatalf("unexpected RPC Stream %q", record.GetStreamId())
		}
		if record.GetFrameType() != remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE {
			zeroV2Bytes(plaintext)
			continue
		}
		response := new(remotev2.RpcResponse)
		err := proto.Unmarshal(plaintext, response)
		zeroV2Bytes(plaintext)
		if err != nil || response.GetOperationId() != operationID {
			controller.t.Fatalf("RPC response = %#v, %v", response, err)
		}
		return response
	}
}

func (controller *v2E2EController) callRPC(root []byte, sequencer *peerv2.Sequencer, channelID, method string, payload []byte) *remotev2.RpcResponse {
	controller.t.Helper()
	streamID := uuid.NewString()
	operationID := uuid.NewString()
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, channelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: channelID, StreamId: streamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: operationID,
	})
	controller.sendRecord(root, sequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, channelID, streamID, &remotev2.RpcRequest{
		OperationId: operationID, AttemptId: uuid.NewString(), Method: method,
		Deadline: timestamppb.New(time.Now().UTC().Add(time.Minute)), Payload: payload,
	})
	return controller.expectRPCResponse(root, sequencer, streamID, operationID)
}

func (controller *v2E2EController) expectRPCEvent(root []byte, sequencer *peerv2.Sequencer, streamID, operationID string) *remotev2.RpcEvent {
	controller.t.Helper()
	record, plaintext := controller.nextRecord(root, sequencer)
	defer zeroV2Bytes(plaintext)
	event := new(remotev2.RpcEvent)
	if record.GetFrameType() != remotev2.FrameType_FRAME_TYPE_RPC_EVENT || record.GetStreamId() != streamID || proto.Unmarshal(plaintext, event) != nil ||
		event.GetOperationId() != operationID || uuid.Validate(event.GetEventId()) != nil {
		controller.t.Fatalf("RPC event record = %#v / %#v", record, event)
	}
	return event
}

func (controller *v2E2EController) expectTerminalFrame(root []byte, sequencer *peerv2.Sequencer, streamID, sessionID string) *remotev2.TerminalStreamFrame {
	controller.t.Helper()
	record, plaintext := controller.nextRecord(root, sequencer)
	defer zeroV2Bytes(plaintext)
	frame := new(remotev2.TerminalStreamFrame)
	if record.GetFrameType() != remotev2.FrameType_FRAME_TYPE_STREAM_DATA || record.GetStreamId() != streamID || proto.Unmarshal(plaintext, frame) != nil ||
		frame.GetSessionId() != sessionID || frame.GetBody() == nil {
		controller.t.Fatalf("Terminal Stream frame = %#v / %#v", record, frame)
	}
	return frame
}

func (controller *v2E2EController) expectFileChunk(root []byte, sequencer *peerv2.Sequencer, streamID, transferID string, index uint64) *remotev2.FileChunk {
	controller.t.Helper()
	record, plaintext := controller.nextRecord(root, sequencer)
	defer zeroV2Bytes(plaintext)
	chunk := new(remotev2.FileChunk)
	if record.GetFrameType() != remotev2.FrameType_FRAME_TYPE_FILE_CHUNK || record.GetStreamId() != streamID || proto.Unmarshal(plaintext, chunk) != nil ||
		chunk.GetTransferId() != transferID || chunk.GetIndex() != index || len(chunk.GetChunkHash()) != sha256.Size {
		controller.t.Fatalf("FileChunk record = %#v / %#v", record, chunk)
	}
	return chunk
}

func (controller *v2E2EController) expectFileAck(root []byte, sequencer *peerv2.Sequencer, streamID, transferID string) *remotev2.FileAck {
	controller.t.Helper()
	record, plaintext := controller.nextRecord(root, sequencer)
	defer zeroV2Bytes(plaintext)
	ack := new(remotev2.FileAck)
	if record.GetFrameType() != remotev2.FrameType_FRAME_TYPE_FILE_ACK || record.GetStreamId() != streamID || proto.Unmarshal(plaintext, ack) != nil || ack.GetTransferId() != transferID {
		controller.t.Fatalf("FileAck record = %#v / %#v", record, ack)
	}
	return ack
}

func forceV2E2ERekey(t *testing.T, controller *v2E2EController, currentRoot []byte, sequencer *peerv2.Sequencer, clientPrivate ed25519.PrivateKey, devicePublic ed25519.PublicKey, boundaries []peerv2.StreamBoundary) []byte {
	t.Helper()
	if controller == nil || controller.keyID == 0 {
		t.Fatal("v2 E2E controller has no active key generation")
	}
	currentKeyID := controller.keyID
	keys, err := peerv2.NewLinkState(controller.linkID, currentRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Close()
	init, err := keys.InitiateRekey(clientPrivate, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	controller.sendRecordWithKey(currentRoot, currentKeyID, sequencer, remotev2.FrameType_FRAME_TYPE_REKEY_INIT, v2ControlChannelID, v2ControlStreamID, &remotev2.RekeyInit{
		LinkId: init.LinkID, RekeyId: init.RekeyID, NextKeyId: init.NextKeyID, EphemeralPublicKey: init.EphemeralPublic, IdentitySignature: init.IdentitySignature,
	})
	ackRecord, ackPayload := controller.nextRecord(currentRoot, sequencer)
	ack := new(remotev2.RekeyAck)
	if ackRecord.GetFrameType() != remotev2.FrameType_FRAME_TYPE_REKEY_ACK || proto.Unmarshal(ackPayload, ack) != nil {
		zeroV2Bytes(ackPayload)
		t.Fatalf("RekeyAck record = %#v", ackRecord)
	}
	zeroV2Bytes(ackPayload)
	if err := keys.ReceiveRekeyAck(peerv2.RekeyAck{
		LinkID: ack.GetLinkId(), RekeyID: ack.GetRekeyId(), NextKeyID: ack.GetNextKeyId(), EphemeralPublic: ack.GetEphemeralPublicKey(), IdentitySignature: ack.GetIdentitySignature(),
	}, devicePublic); err != nil {
		t.Fatal(err)
	}
	commit, err := keys.CommitRekey(boundaries)
	if err != nil {
		t.Fatal(err)
	}
	protoBoundaries := make([]*remotev2.StreamKeyBoundary, 0, len(commit.Boundaries))
	for _, boundary := range commit.Boundaries {
		protoBoundaries = append(protoBoundaries, &remotev2.StreamKeyBoundary{StreamId: boundary.StreamID, NextSequence: boundary.NextSequence})
	}
	controller.sendRecordWithKey(currentRoot, currentKeyID, sequencer, remotev2.FrameType_FRAME_TYPE_REKEY_COMMIT, v2ControlChannelID, v2ControlStreamID, &remotev2.RekeyCommit{
		LinkId: commit.LinkID, RekeyId: commit.RekeyID, NextKeyId: commit.NextKeyID, Boundaries: protoBoundaries,
	})
	nextRoot, err := keys.RootKey(commit.NextKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.RootKey(currentKeyID); err != nil {
		zeroV2Bytes(nextRoot)
		t.Fatalf("controller did not retain prior rekey generation: %v", err)
	}
	controller.keyID = commit.NextKeyID
	return nextRoot
}

func appendV2E2EAgentEvent(t *testing.T, ctx context.Context, state *agentState, event agentEventRecord) agentEventRecord {
	t.Helper()
	if state == nil || state.business == nil {
		t.Fatal("Agent business state is unavailable")
	}
	state.business.mu.Lock()
	defer state.business.mu.Unlock()
	db, err := state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stored, err := appendAgentEvent(ctx, tx, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return stored
}

func randomV2E2EBytes(t *testing.T, size int) []byte {
	t.Helper()
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return value
}
