package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	peerv2 "github.com/wenzwork/wenzwork-web/server/internal/peerprotocol/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

func TestV2AgentEventAckTreatsStaleWatermarksAsIdempotent(t *testing.T) {
	subscriptionID := uuid.NewString()
	channelID := uuid.NewString()
	streamID := uuid.NewString()
	subscription := &v2AgentEventSubscription{
		subscriptionID: subscriptionID,
		channelID:      channelID,
		streamID:       streamID,
		highWatermark:  10,
		acknowledged:   5,
	}
	link := &v2AgentLink{events: map[string]*v2AgentEventSubscription{subscriptionID: subscription}}
	record := &remotev2.EncryptedRecord{ChannelId: channelID, StreamId: streamID}

	stale, err := proto.Marshal(&remotev2.EventAck{SubscriptionId: subscriptionID, HighWatermark: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.handleEventAck(record, stale); err != nil {
		t.Fatalf("stale Event ACK closed a healthy Stream: %v", err)
	}
	if subscription.acknowledged != 5 {
		t.Fatalf("stale Event ACK moved cursor to %d", subscription.acknowledged)
	}

	future, err := proto.Marshal(&remotev2.EventAck{SubscriptionId: subscriptionID, HighWatermark: 11})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.handleEventAck(record, future); !errors.Is(err, errV2AgentLink) {
		t.Fatalf("future Event ACK error = %v", err)
	}
}

func TestV2AgentCarrierResumeGatesLiveEventsUntilDurableReplay(t *testing.T) {
	subscriptionID := uuid.NewString()
	channelID := uuid.NewString()
	streamID := uuid.NewString()
	subscription := &v2AgentEventSubscription{
		subscriptionID: subscriptionID,
		channelID:      channelID,
		streamID:       streamID,
		highWatermark:  9,
		acknowledged:   3,
	}
	link := &v2AgentLink{events: map[string]*v2AgentEventSubscription{subscriptionID: subscription}}
	link.applyCarrierResume([]*remotev2.StreamAck{{
		ChannelId: channelID, StreamId: streamID, AcknowledgedSequence: 5,
	}})

	subscription.sendMu.Lock()
	if !subscription.resumePending || subscription.resumeReady == nil || subscription.acknowledged != 5 {
		t.Fatalf("Carrier resume barrier = pending %v, ready %v, acknowledged %d", subscription.resumePending, subscription.resumeReady != nil, subscription.acknowledged)
	}
	subscription.sendMu.Unlock()

	live := make(chan struct{}, 1)
	go func() {
		if link.lockLiveEventSend(t.Context(), subscription) {
			live <- struct{}{}
			subscription.sendMu.Unlock()
		}
	}()
	select {
	case <-live:
		t.Fatal("live Event overtook the pending durable replay")
	case <-time.After(25 * time.Millisecond):
	}

	// This is the successful end of EVENT_RESUME's serialized replay section.
	subscription.sendMu.Lock()
	finishEventResumeLocked(subscription)
	subscription.sendMu.Unlock()
	select {
	case <-live:
	case <-time.After(time.Second):
		t.Fatal("live Event did not resume after the durable replay")
	}
}

func TestV2AgentResourceSnapshotIncludesBoundedQueues(t *testing.T) {
	subscriber := &agentEventSubscriber{queueBytes: 4096}
	link := &v2AgentLink{
		events: map[string]*v2AgentEventSubscription{
			uuid.NewString(): {subscriber: subscriber},
		},
		channels:   make(map[string]*v2AgentChannel),
		operations: make(map[string]v2AgentOperation),
		files:      make(map[string]*v2AgentFileTransfer),
		downloads:  make(map[string]*v2AgentDownloadTransfer),
	}
	carrier := &v2AgentCarrier{writer: &v2AgentWriter{
		bytes:  map[v2AgentPriority]int{v2AgentControl: 1024, v2AgentBulk: 2048},
		frames: map[v2AgentPriority]int{v2AgentControl: 1, v2AgentBulk: 2},
	}}
	registry := newV2AgentLinkRegistry(nil)
	registry.carrier = carrier
	registry.links[uuid.NewString()] = link

	snapshot := registry.resourceSnapshot()
	if snapshot.CarrierQueuedBytes != 3072 || snapshot.CarrierQueuedFrames != 3 ||
		snapshot.EventSubscriptionCount != 1 || snapshot.EventQueuedBytes != 4096 {
		t.Fatalf("resource snapshot = %+v", snapshot)
	}
}

func TestV2AgentDownloadAckUsesConstantMemoryCursor(t *testing.T) {
	transferID := uuid.NewString()
	channelID := uuid.NewString()
	streamID := uuid.NewString()
	transfer := &v2AgentDownloadTransfer{
		transferID:  transferID,
		channelID:   channelID,
		streamID:    streamID,
		totalLength: 4 * fileChunkBytes,
		chunkSize:   fileChunkBytes,
		nextIndex:   2,
		sentIndex:   2,
		sent:        true,
	}
	link := &v2AgentLink{downloads: map[string]*v2AgentDownloadTransfer{transferID: transfer}}
	record := &remotev2.EncryptedRecord{ChannelId: channelID, StreamId: streamID}

	stale, err := proto.Marshal(&remotev2.FileAck{TransferId: transferID, ConfirmedIndexes: []uint64{1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.handleFileAck(t.Context(), record, stale); err != nil {
		t.Fatalf("stale File ACK was not idempotent: %v", err)
	}
	if transfer.nextIndex != 2 || !transfer.sent || transfer.sentIndex != 2 {
		t.Fatalf("stale File ACK changed cursor: %+v", transfer)
	}

	future, err := proto.Marshal(&remotev2.FileAck{TransferId: transferID, ConfirmedIndexes: []uint64{3}})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.handleFileAck(t.Context(), record, future); !errors.Is(err, errV2AgentLink) {
		t.Fatalf("future File ACK error = %v", err)
	}
}

func TestV2AgentManagedUploadAcceptsNonZeroResumeOffset(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.send")
	transferID := uuid.NewString()
	payload := bytes.Repeat([]byte{0x5a}, fileChunkBytes*2)
	digest := sha256.Sum256(payload)
	mustFileRPC(t, dispatch, "file.upload.prepare", rpcInput{
		"transferId": transferID,
		"path":       "resume.bin",
		"size":       float64(len(payload)),
		"sha256":     base64.RawURLEncoding.EncodeToString(digest[:]),
	})
	partialOffset := 123
	mustFileRPC(t, dispatch, "file.upload.chunk", uploadChunkInput(transferID, 0, payload[:partialOffset]))
	manifest := &remotev2.FileManifest{
		TransferId:         transferID,
		TotalLength:        uint64(len(payload)),
		ChunkSize:          fileChunkBytes,
		Sha256:             digest[:],
		RelativePathHandle: transferID,
	}
	link := &v2AgentLink{state: dispatch.state}
	file, _, acceptedOffset, err := link.managedV2UploadFile(dispatch.requestProjectID, manifest)
	if err != nil {
		t.Fatalf("resume managed upload: %v", err)
	}
	defer file.Close()
	if acceptedOffset != uint64(partialOffset) {
		t.Fatalf("accepted offset = %d, want %d", acceptedOffset, partialOffset)
	}
}

func TestV2AgentResourceSnapshotTracksAndReleasesStreamState(t *testing.T) {
	state := &agentState{}
	registry := newV2AgentLinkRegistry(state)
	sequencer := peerv2.NewSequencer(64)
	streamID := uuid.NewString()
	channelID := uuid.NewString()
	if err := sequencer.OpenStream(streamID); err != nil {
		t.Fatal(err)
	}
	if _, err := sequencer.Next(1, peerv2.DirectionDeviceToClient, streamID); err != nil {
		t.Fatal(err)
	}
	if err := sequencer.AcceptInbound(peerv2.RecordMetadata{
		LinkID: uuid.NewString(), ChannelID: channelID, StreamID: streamID, KeyID: 1,
		Direction: peerv2.DirectionClientToDevice, FrameType: peerv2.FrameRPCRequest, StreamSequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	link := &v2AgentLink{
		id: uuid.NewString(), sequencer: sequencer, sendLocks: make(map[string]*v2AgentSendLock),
		channels: map[string]*v2AgentChannel{
			channelID: {id: channelID, streams: map[string]*v2AgentStream{streamID: {id: streamID}}},
		},
		operations: make(map[string]v2AgentOperation), files: make(map[string]*v2AgentFileTransfer),
		downloads: make(map[string]*v2AgentDownloadTransfer), events: make(map[string]*v2AgentEventSubscription),
	}
	registry.links[link.id] = link

	active := state.v2ResourceSnapshot()
	if active.LinkCount != 1 || active.ChannelCount != 1 || active.ActiveStreamCount != 1 ||
		active.SequencerActiveStreams != 1 || active.SequencerUsedStreamIDs != 1 ||
		active.SequencerOutboundEntries != 1 || active.SequencerInboundEntries != 1 ||
		active.SequencerStreamHardLimit != peerv2.DefaultSequencerStreamLimit {
		t.Fatalf("active resource snapshot = %+v", active)
	}
	link.closeStream(channelID, streamID)
	closed := state.v2ResourceSnapshot()
	if closed.ActiveStreamCount != 0 || closed.SequencerActiveStreams != 0 ||
		closed.SequencerOutboundEntries != 0 || closed.SequencerInboundEntries != 0 || closed.SequencerTombstones == 0 ||
		closed.SequencerTombstones > closed.SequencerTombstoneHardLimit || closed.SequencerUsedStreamIDs != 1 {
		t.Fatalf("closed resource snapshot = %+v", closed)
	}
}

func TestV2AgentStreamIDCannotBeReopenedAfterKeyStateRetires(t *testing.T) {
	channelID := uuid.NewString()
	otherChannelID := uuid.NewString()
	streamID := uuid.NewString()
	link := &v2AgentLink{
		id: uuid.NewString(), active: true, context: context.Background(),
		sequencer: peerv2.NewSequencerWithResourceLimits(64, 16, 8, 8),
		channels: map[string]*v2AgentChannel{
			channelID:      {id: channelID, streams: make(map[string]*v2AgentStream)},
			otherChannelID: {id: otherChannelID, streams: make(map[string]*v2AgentStream)},
		},
		sendLocks: make(map[string]*v2AgentSendLock),
	}
	record := &remotev2.EncryptedRecord{ChannelId: channelID}
	openPayload := func() []byte {
		payload, err := proto.Marshal(&remotev2.StreamOpen{
			ChannelId: channelID, StreamId: streamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC,
		})
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	if err := link.handleStreamOpen(record, openPayload()); err != nil {
		t.Fatal(err)
	}
	if err := link.handleStreamOpen(record, openPayload()); err != nil {
		t.Fatalf("active metadata-identical Stream assertion = %v", err)
	}
	changedPayload, err := proto.Marshal(&remotev2.StreamOpen{
		ChannelId: channelID, StreamId: streamID, Kind: remotev2.StreamKind_STREAM_KIND_FILE,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.handleStreamOpen(record, changedPayload); !errors.Is(err, peerv2.ErrStreamReuse) {
		t.Fatalf("active changed-metadata Stream reuse error = %v, want ErrStreamReuse", err)
	}
	otherPayload, err := proto.Marshal(&remotev2.StreamOpen{
		ChannelId: otherChannelID, StreamId: streamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.handleStreamOpen(&remotev2.EncryptedRecord{ChannelId: otherChannelID}, otherPayload); !errors.Is(err, peerv2.ErrStreamReuse) {
		t.Fatalf("cross-Channel Stream reuse error = %v, want ErrStreamReuse", err)
	}
	link.closeStream(channelID, streamID)
	link.sequencer.RetireKey(1)
	if err := link.handleStreamOpen(record, openPayload()); !errors.Is(err, peerv2.ErrStreamReuse) {
		t.Fatalf("reopened Stream error = %v, want ErrStreamReuse", err)
	}
}

func TestV2AuthenticatedLinkFromSameControllerSupersedesOrphan(t *testing.T) {
	controllerID := uuid.NewString()
	orphan := &v2AgentLink{
		id:      uuid.NewString(),
		binding: peerv2.HandshakeBinding{ClientID: controllerID},
		active:  true,
	}
	replacement := &v2AgentLink{
		id:      uuid.NewString(),
		binding: peerv2.HandshakeBinding{ClientID: controllerID},
	}
	registry := &v2AgentLinkRegistry{links: map[string]*v2AgentLink{orphan.id: orphan}}

	replaced, err := registry.installAuthenticatedLink(replacement)
	if err != nil {
		t.Fatalf("installAuthenticatedLink() error = %v", err)
	}
	if replaced != orphan || registry.get(replacement.id) != replacement || registry.get(orphan.id) != nil {
		t.Fatal("fresh proof-bound Link did not atomically supersede the same Controller's orphan")
	}
}

func TestV2AuthenticatedLinksFromDifferentControllersCoexist(t *testing.T) {
	existing := &v2AgentLink{
		id:      uuid.NewString(),
		binding: peerv2.HandshakeBinding{ClientID: uuid.NewString()},
	}
	candidate := &v2AgentLink{
		id:      uuid.NewString(),
		binding: peerv2.HandshakeBinding{ClientID: uuid.NewString()},
	}
	registry := &v2AgentLinkRegistry{links: map[string]*v2AgentLink{existing.id: existing}}

	replaced, err := registry.installAuthenticatedLink(candidate)
	if err != nil || replaced != nil {
		t.Fatalf("installAuthenticatedLink() = %#v, %v; want an independent Link", replaced, err)
	}
	if registry.get(existing.id) != existing || registry.get(candidate.id) != candidate {
		t.Fatal("independently authorised Controllers did not retain separate Links")
	}
}

func TestV2AuthenticatedLinkReplacementPreservesOtherControllers(t *testing.T) {
	controllerID := uuid.NewString()
	sameController := &v2AgentLink{
		id:      uuid.NewString(),
		binding: peerv2.HandshakeBinding{ClientID: controllerID},
	}
	otherController := &v2AgentLink{
		id:      uuid.NewString(),
		binding: peerv2.HandshakeBinding{ClientID: uuid.NewString()},
	}
	candidate := &v2AgentLink{
		id:      uuid.NewString(),
		binding: peerv2.HandshakeBinding{ClientID: controllerID},
	}
	registry := &v2AgentLinkRegistry{links: map[string]*v2AgentLink{
		sameController.id:  sameController,
		otherController.id: otherController,
	}}

	replaced, err := registry.installAuthenticatedLink(candidate)
	if err != nil || replaced != sameController {
		t.Fatalf("installAuthenticatedLink() = %#v, %v; want same-Controller replacement", replaced, err)
	}
	if registry.get(sameController.id) != nil ||
		registry.get(otherController.id) != otherController ||
		registry.get(candidate.id) != candidate {
		t.Fatal("same-Controller replacement changed another Controller's Link")
	}
}

func TestV2AuthenticatedLinkConflictDoesNotPartiallyMutateRegistry(t *testing.T) {
	controllerID := uuid.NewString()
	first := &v2AgentLink{id: uuid.NewString(), binding: peerv2.HandshakeBinding{ClientID: controllerID}}
	second := &v2AgentLink{id: uuid.NewString(), binding: peerv2.HandshakeBinding{ClientID: controllerID}}
	candidate := &v2AgentLink{id: uuid.NewString(), binding: peerv2.HandshakeBinding{ClientID: controllerID}}
	registry := &v2AgentLinkRegistry{links: map[string]*v2AgentLink{
		first.id: first, second.id: second,
	}}

	replaced, err := registry.installAuthenticatedLink(candidate)
	if replaced != nil || !errors.Is(err, errV2AgentLinkConflict) {
		t.Fatalf("installAuthenticatedLink() = %#v, %v; want atomic registry conflict", replaced, err)
	}
	if registry.get(first.id) != first || registry.get(second.id) != second || registry.get(candidate.id) != nil {
		t.Fatal("a conflicting replacement partially mutated the Link registry")
	}
	if got := v2AgentLinkRejectCode(err); got != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_AUTHENTICATION_FAILED {
		t.Fatalf("Controller registry conflict code = %v", got)
	}
}

func TestV2AuthenticatedLinkCapacityIsBounded(t *testing.T) {
	links := make(map[string]*v2AgentLink, v2MaximumLinksPerDevice)
	for range v2MaximumLinksPerDevice {
		link := &v2AgentLink{id: uuid.NewString(), binding: peerv2.HandshakeBinding{ClientID: uuid.NewString()}}
		links[link.id] = link
	}
	candidate := &v2AgentLink{id: uuid.NewString(), binding: peerv2.HandshakeBinding{ClientID: uuid.NewString()}}
	registry := &v2AgentLinkRegistry{links: links}

	replaced, err := registry.installAuthenticatedLink(candidate)
	if replaced != nil || !errors.Is(err, errV2AgentLinkCapacity) {
		t.Fatalf("installAuthenticatedLink() = %#v, %v; want bounded capacity", replaced, err)
	}
	if len(registry.links) != v2MaximumLinksPerDevice || registry.get(candidate.id) != nil {
		t.Fatal("capacity rejection changed the active Link registry")
	}
	if got := v2AgentLinkRejectCode(err); got != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_BACKPRESSURE {
		t.Fatalf("Link capacity code = %v", got)
	}
	if got := v2AgentLinkRejectCode(errV2AgentLinkAuth); got != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_IDENTITY_INVALID {
		t.Fatalf("identity failure code = %v", got)
	}
	if got := v2AgentLinkRejectCode(errV2AgentLink); got != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID {
		t.Fatalf("frame failure code = %v", got)
	}
}

func TestValidateV2ChannelScopesSeparatesDeviceAndProjectCapabilities(t *testing.T) {
	allowed := map[string]struct{}{
		"remote.peer.query":        {},
		"remote.peer.ai.config":    {},
		"remote.peer.ai.chat":      {},
		"remote.peer.events":       {},
		"remote.peer.file.send":    {},
		"remote.peer.file.receive": {},
	}
	for _, scope := range []string{"remote.peer.query", "remote.peer.ai.config"} {
		granted, err := validateV2ChannelScopes(
			allowed,
			remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY,
			"",
			[]string{scope},
		)
		if err != nil || len(granted) != 1 {
			t.Fatalf("device scope %q = %#v, %v", scope, granted, err)
		}
	}
	for _, test := range []struct {
		name      string
		projectID string
		scopes    []string
	}{
		{name: "device cannot bind project", projectID: uuid.NewString(), scopes: []string{"remote.peer.query"}},
		{name: "device cannot combine scopes", scopes: []string{"remote.peer.query", "remote.peer.ai.config"}},
		{name: "device cannot request project scope", scopes: []string{"remote.peer.ai.chat"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateV2ChannelScopes(allowed, remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY, test.projectID, test.scopes); err == nil {
				t.Fatal("device Channel was granted an invalid scope binding")
			}
		})
	}
	projectID := uuid.NewString()
	granted, err := validateV2ChannelScopes(
		allowed,
		remotev2.ChannelKind_CHANNEL_KIND_PROJECT,
		projectID,
		[]string{"remote.peer.ai.chat", "remote.peer.file.send"},
	)
	if err != nil || len(granted) != 2 {
		t.Fatalf("project scopes = %#v, %v", granted, err)
	}
	if _, err := validateV2ChannelScopes(allowed, remotev2.ChannelKind_CHANNEL_KIND_PROJECT, projectID, []string{"remote.peer.query"}); err == nil {
		t.Fatal("project Channel was granted device query scope")
	}
}

func TestV2ChannelMethodAuthorizationKeepsCapabilitiesDeviceScopedAndSupportsTaskAliases(t *testing.T) {
	query := &v2AgentChannel{
		kind:   remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY,
		scopes: map[string]struct{}{"remote.peer.query": {}},
	}
	if !v2ChannelAllowsMethod(query, "agent.capabilities.get") {
		t.Fatal("device query Channel did not permit capability discovery")
	}
	if v2ChannelAllowsMethod(query, "conversation.list") {
		t.Fatal("device query Channel permitted a project AI method")
	}
	project := &v2AgentChannel{
		kind: remotev2.ChannelKind_CHANNEL_KIND_PROJECT, projectID: uuid.NewString(),
		scopes: map[string]struct{}{"remote.peer.task.control": {}},
	}
	if !v2ChannelAllowsMethod(project, "task.list") || !v2ChannelAllowsMethod(project, "task.create") {
		t.Fatal("task read/write aliases were not authorized by the project task Channel")
	}
	if got := v2ChannelScopeForMethod(project, "task.list"); got != "remote.peer.task.control" {
		t.Fatalf("task.list dispatch scope = %q, want remote.peer.task.control", got)
	}
	if v2ChannelAllowsMethod(project, "agent.capabilities.get") {
		t.Fatal("project task Channel permitted device capability discovery")
	}
}

func TestV2StreamCloseUsesEncryptedPayloadTarget(t *testing.T) {
	channelID, streamID := uuid.NewString(), uuid.NewString()
	cancelled := false
	link := &v2AgentLink{channels: map[string]*v2AgentChannel{
		channelID: {
			id: channelID,
			streams: map[string]*v2AgentStream{
				streamID: {id: streamID, cancel: func() { cancelled = true }},
			},
		},
	}}
	plaintext, err := proto.Marshal(&remotev2.StreamClose{
		ChannelId: channelID, StreamId: streamID, Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_CANCELLED,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.handleStreamClose(&remotev2.EncryptedRecord{
		ChannelId: channelID, StreamId: v2ChannelControlStreamID,
	}, plaintext); err != nil {
		t.Fatalf("handleStreamClose() error = %v", err)
	}
	if !cancelled {
		t.Fatal("StreamClose did not cancel its payload target")
	}
	if _, found := link.channels[channelID].streams[streamID]; found {
		t.Fatal("StreamClose retained its payload target")
	}
}

func TestV2LinkRecoveryWindowSupportsShortAndFiveMinuteCarrierOutages(t *testing.T) {
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	link := &v2AgentLink{suspendedAt: now}
	for _, outage := range []time.Duration{time.Second, 30 * time.Second, v2LinkRecoveryTTL - time.Nanosecond} {
		if link.expiredRecovery(now.Add(outage)) {
			t.Fatalf("recovery unexpectedly expired after %s", outage)
		}
	}
	if !link.expiredRecovery(now.Add(v2LinkRecoveryTTL)) {
		t.Fatalf("recovery remains valid at the five-minute TTL")
	}
	link.bindCarrier(&v2AgentCarrier{})
	if link.expiredRecovery(now.Add(24 * time.Hour)) {
		t.Fatal("binding a new Carrier did not clear the suspended Link state")
	}
}

func TestV2RenewableLinkLeaseStillCapsDisconnectedRecoveryAtFiveMinutes(t *testing.T) {
	now := time.Date(2026, time.August, 21, 15, 0, 0, 0, time.UTC)
	link := &v2AgentLink{active: true, suspendedAt: now, channels: make(map[string]*v2AgentChannel)}
	if !link.renewLease(1, now) {
		t.Fatal("first encrypted Link lease renewal was rejected")
	}
	firstExpiry := now.Add(v2LinkLeaseDuration)
	if !link.leaseEnabled || link.leaseRenewalSequence != 1 || !link.leaseExpiresAt.Equal(firstExpiry) {
		t.Fatalf("first lease = enabled:%t sequence:%d expiry:%s", link.leaseEnabled, link.leaseRenewalSequence, link.leaseExpiresAt)
	}
	if !link.renewLease(1, now.Add(time.Minute)) || !link.leaseExpiresAt.Equal(firstExpiry) {
		t.Fatal("idempotent renewal changed or rejected the existing lease")
	}
	if link.renewLease(3, now.Add(2*time.Minute)) {
		t.Fatal("renewal sequence jump was accepted")
	}
	secondRenewal := now.Add(v2LinkLeaseRenewalInterval)
	if !link.renewLease(2, secondRenewal) {
		t.Fatal("second encrypted Link lease renewal was rejected")
	}
	if link.expiredRecovery(now.Add(v2LinkRecoveryTTL - time.Nanosecond)) {
		t.Fatal("renewed Link expired before its five-minute disconnect cache")
	}
	if !link.expiredRecovery(now.Add(v2LinkRecoveryTTL)) {
		t.Fatal("renewed Link remained recoverable after the five-minute disconnect cache")
	}
	link.bindCarrier(&v2AgentCarrier{})
	if link.expiredRecovery(secondRenewal.Add(v2LinkLeaseDuration - time.Nanosecond)) {
		t.Fatal("active renewed Link expired before its 90-minute authorization lease")
	}
	if !link.expiredRecovery(secondRenewal.Add(v2LinkLeaseDuration)) {
		t.Fatal("active renewed Link remained valid at lease expiry")
	}
	if link.renewLease(3, secondRenewal.Add(v2LinkLeaseDuration)) {
		t.Fatal("an expired Device lease was renewed")
	}
	link.close()
}

func TestV2AgentRegistryDropsExpiredLinksBeforeCarrierRebind(t *testing.T) {
	link := &v2AgentLink{
		id: uuid.NewString(), suspendedAt: time.Now().UTC().Add(-v2LinkRecoveryTTL),
		channels: make(map[string]*v2AgentChannel), sendLocks: make(map[string]*v2AgentSendLock),
	}
	registry := &v2AgentLinkRegistry{links: map[string]*v2AgentLink{link.id: link}}
	registry.bindCarrier(&v2AgentCarrier{})
	if registry.get(link.id) != nil || !link.closed {
		t.Fatal("expired Link survived a replacement Carrier bind")
	}
}

func TestV2PersistentGrantAcceptsOnlyMonotonicDeviceEpoch(t *testing.T) {
	claims := remoteauth.DeviceLinkGrantClaims{
		TargetConnectionEpoch:  7,
		MaximumLifetimeSeconds: 0,
		ExpiresAt:              remoteauth.PersistentDeviceLinkGrantExpiresAtUnix,
	}
	if !v2DeviceLinkGrantMatchesCarrierEpoch(claims, 7) || !v2DeviceLinkGrantMatchesCarrierEpoch(claims, 8) {
		t.Fatal("persistent Grant did not survive a monotonic Device Carrier handoff")
	}
	if v2DeviceLinkGrantMatchesCarrierEpoch(claims, 6) {
		t.Fatal("persistent Grant accepted a Device epoch rollback")
	}
	claims.MaximumLifetimeSeconds = 90
	claims.ExpiresAt = time.Now().Add(90 * time.Second).Unix()
	if v2DeviceLinkGrantMatchesCarrierEpoch(claims, 8) {
		t.Fatal("bounded legacy Grant ignored its exact Device epoch binding")
	}
}

func TestV2AgentCarrierRejectionIsScopedToLinkOrStream(t *testing.T) {
	channelID, rejectedStream, siblingStream := uuid.NewString(), uuid.NewString(), uuid.NewString()
	rejectedCancelled, siblingCancelled := false, false
	link := &v2AgentLink{
		id: uuid.NewString(), sequencer: peerv2.NewSequencer(16),
		channels: map[string]*v2AgentChannel{channelID: {id: channelID, streams: map[string]*v2AgentStream{
			rejectedStream: {id: rejectedStream, cancel: func() { rejectedCancelled = true }},
			siblingStream:  {id: siblingStream, cancel: func() { siblingCancelled = true }},
		}}},
		sendLocks: make(map[string]*v2AgentSendLock),
	}
	if err := link.sequencer.OpenStream(rejectedStream); err != nil {
		t.Fatal(err)
	}
	if err := link.sequencer.OpenStream(siblingStream); err != nil {
		t.Fatal(err)
	}
	registry := &v2AgentLinkRegistry{links: map[string]*v2AgentLink{link.id: link}}
	if err := registry.handleStreamRejected(&remotev2.CarrierStreamRejected{
		LinkId: link.id, ChannelId: channelID, StreamId: rejectedStream,
		Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_BACKPRESSURE,
	}); err != nil {
		t.Fatal(err)
	}
	if !rejectedCancelled || siblingCancelled || registry.get(link.id) != link {
		t.Fatal("Stream backpressure escaped its Stream boundary")
	}
	if err := registry.handleStreamRejected(&remotev2.CarrierStreamRejected{
		LinkId: link.id, Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED,
	}); err != nil {
		t.Fatal(err)
	}
	if registry.get(link.id) != nil || !siblingCancelled {
		t.Fatal("Link-scoped expiry did not release the remaining Link resources")
	}
}

func TestV2AgentWriterDropsCancelledQueuedRequest(t *testing.T) {
	writer := &v2AgentWriter{
		queues: make(map[v2AgentPriority][]*v2AgentWriteRequest), bytes: make(map[v2AgentPriority]int), frames: make(map[v2AgentPriority]int),
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := writer.enqueue(ctx, &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{MonotonicMillis: 1}}}, v2AgentControl)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enqueue = %v", err)
	}
	request, ok := writer.dequeue()
	if !ok || request == nil || !request.cancelled.Load() {
		t.Fatal("cancelled queued request was not marked for suppression")
	}
	if writer.frames[v2AgentControl] != 0 || writer.bytes[v2AgentControl] != 0 {
		t.Fatal("cancelled queued request retained queue budget")
	}
}

func TestV2ReconnectJitterHasFloorAndHonorsServerMinimum(t *testing.T) {
	if got := v2FullJitter(50 * time.Millisecond); got != 50*time.Millisecond {
		t.Fatalf("small jitter bound = %s", got)
	}
	for range 1000 {
		got := v2FullJitter(500 * time.Millisecond)
		if got < 100*time.Millisecond || got > 500*time.Millisecond {
			t.Fatalf("jitter = %s, want [100ms, 500ms]", got)
		}
	}
	httpFailure := &controlHTTPError{Status: http.StatusServiceUnavailable, RetryAfter: 45 * time.Second}
	if got := v2FailureRetryAfter(httpFailure); got != 45*time.Second {
		t.Fatalf("HTTP retry minimum = %s", got)
	}
	goAway := &v2AgentGoAwayError{reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_ROUTE_STALE, retryAfter: 90 * time.Second}
	if got := v2FailureRetryAfter(goAway); got != 90*time.Second {
		t.Fatalf("GOAWAY retry minimum = %s", got)
	}
}

func TestV2RelayDialFailurePreservesRetryAfter(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header)}
	response.Header.Set("Retry-After", "45")
	err := v2RelayDialFailure(response)
	var failure *controlHTTPError
	if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable || failure.RetryAfter != 45*time.Second {
		t.Fatalf("Relay dial failure = %#v / %v", failure, err)
	}
}

func TestV2AllocationRetryReusesEpochAndIdempotencyKey(t *testing.T) {
	root := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(root, "agent.json"), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	var diagnosticsMu sync.Mutex
	diagnostics := make([]deviceConnectionDiagnostic, 0, 8)
	state.connectionDiagnosticSink = func(diagnostic deviceConnectionDiagnostic) {
		diagnosticsMu.Lock()
		diagnostics = append(diagnostics, diagnostic)
		diagnosticsMu.Unlock()
	}
	store, err := loadControlState(state)
	if err != nil {
		t.Fatal(err)
	}
	type observedRequest struct {
		epoch uint64
		key   string
	}
	var mu sync.Mutex
	observed := make([]observedRequest, 0, 4)
	third := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			ConnectionEpoch uint64 `json:"connectionEpoch"`
		}
		if request.URL.Path != "/v1/device/relay-allocations" || json.NewDecoder(request.Body).Decode(&body) != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		observed = append(observed, observedRequest{epoch: body.ConnectionEpoch, key: request.Header.Get("Idempotency-Key")})
		count := len(observed)
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/problem+json")
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"code":"relay_unavailable"}`))
		if count >= 3 {
			select {
			case third <- struct{}{}:
			default:
			}
		}
	}))
	defer server.Close()
	manager, err := newDeviceTokenManager(server.Client(), mustURL(t, server.URL), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.acceptInitial(deviceTokenSet{
		AccessToken: "access", ExpiresIn: 600, RefreshToken: "refresh", RefreshExpiresIn: 1200,
		SessionID: uuid.New(), Scope: "remote.connect",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runTargetRelayLoopV2(ctx, server.Client(), manager, state) }()
	select {
	case <-third:
		cancel()
	case <-ctx.Done():
		t.Fatal("allocation retry did not reach a second outer attempt")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Relay loop exit = %v", err)
	}
	mu.Lock()
	capturedRequests := append([]observedRequest(nil), observed...)
	mu.Unlock()
	if len(capturedRequests) < 3 || capturedRequests[0].epoch == 0 || capturedRequests[0].key == "" {
		t.Fatalf("observed allocation requests = %+v", capturedRequests)
	}
	for _, request := range capturedRequests[1:] {
		if request != capturedRequests[0] {
			t.Fatalf("retry changed allocation identity: %+v", capturedRequests)
		}
	}
	if state.ConnectionEpoch != capturedRequests[0].epoch {
		t.Fatalf("persisted epoch = %d, requests = %+v", state.ConnectionEpoch, capturedRequests)
	}
	diagnosticsMu.Lock()
	capturedDiagnostics := append([]deviceConnectionDiagnostic(nil), diagnostics...)
	diagnosticsMu.Unlock()
	foundAllocationFailure := false
	for _, diagnostic := range capturedDiagnostics {
		if diagnostic.Event == "relay_allocation_failed" && diagnostic.Reason == "relay_unavailable" {
			foundAllocationFailure = true
			break
		}
	}
	if !foundAllocationFailure {
		t.Fatalf("connection diagnostics = %+v, want relay_allocation_failed/relay_unavailable", capturedDiagnostics)
	}
}

func TestV2MalformedBusinessFrameClosesOnlyItsStream(t *testing.T) {
	linkID, channelID := uuid.NewString(), uuid.NewString()
	badStreamID, healthyStreamID := uuid.NewString(), uuid.NewString()
	root := bytes.Repeat([]byte{0x41}, peerv2.RootKeySize)
	keys, err := peerv2.NewLinkState(linkID, root)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Close()
	link := &v2AgentLink{
		id: linkID, keys: keys, sequencer: peerv2.NewSequencer(16), active: true,
		channels: map[string]*v2AgentChannel{channelID: {
			id: channelID,
			streams: map[string]*v2AgentStream{
				badStreamID:     {id: badStreamID, kind: remotev2.StreamKind_STREAM_KIND_FILE},
				healthyStreamID: {id: healthyStreamID, kind: remotev2.StreamKind_STREAM_KIND_RPC},
			},
		}},
	}
	metadata := peerv2.RecordMetadata{
		LinkID: linkID, ChannelID: channelID, StreamID: badStreamID, KeyID: 1,
		Direction: peerv2.DirectionClientToDevice, FrameType: peerv2.FrameFileManifest, StreamSequence: 1,
	}
	recordKey, err := v2AgentRecordKey(root, metadata)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := peerv2.Seal(recordKey, []byte{0x80}, metadata)
	zeroV2Bytes(recordKey)
	if err != nil {
		t.Fatal(err)
	}
	err = link.handleEnvelope(context.Background(), &remotev2.LinkEnvelope{
		LinkId: linkID,
		Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
			LinkId: linkID, ChannelId: channelID, StreamId: badStreamID, KeyId: 1,
			Direction: remotev2.Direction_DIRECTION_CLIENT_TO_DEVICE, FrameType: remotev2.FrameType_FRAME_TYPE_FILE_MANIFEST,
			StreamSequence: 1, Ciphertext: ciphertext,
		}},
	})
	if err != nil {
		t.Fatalf("handleEnvelope() = %v", err)
	}
	if _, exists := link.channels[channelID].streams[badStreamID]; exists {
		t.Fatal("malformed File Stream remained open")
	}
	if _, exists := link.channels[channelID].streams[healthyStreamID]; !exists || !link.isActive() {
		t.Fatal("malformed File Stream poisoned a healthy RPC Stream or Link")
	}
}

func TestV2ChannelCancellationDoesNotTouchSiblingChannel(t *testing.T) {
	cancelledA, cancelledB := false, false
	channelA, channelB := uuid.NewString(), uuid.NewString()
	link := &v2AgentLink{channels: map[string]*v2AgentChannel{
		channelA: {id: channelA, streams: map[string]*v2AgentStream{uuid.NewString(): {cancel: func() { cancelledA = true }}}},
		channelB: {id: channelB, streams: map[string]*v2AgentStream{uuid.NewString(): {cancel: func() { cancelledB = true }}}},
	}}
	link.closeChannel(channelA)
	if !cancelledA || cancelledB {
		t.Fatalf("Channel cancellation = A:%t B:%t, want true/false", cancelledA, cancelledB)
	}
	if _, exists := link.channels[channelA]; exists {
		t.Fatal("cancelled Channel remained in Link")
	}
	if _, exists := link.channels[channelB]; !exists {
		t.Fatal("sibling Channel was removed")
	}
}

func TestV2AIAndInteractiveRPCsUseLiveMessageStreams(t *testing.T) {
	for _, method := range []string{
		"conversation.send",
		"conversation.chat.send",
		"chat.send",
		"conversation.regenerate",
		"conversation.generation.attach",
		"terminal.attach",
	} {
		if !v2RPCLiveMethod(method) {
			t.Fatalf("%s must emit RPC_EVENT records while the operation is running", method)
		}
	}
	if v2RPCLiveMethod("conversation.get") {
		t.Fatal("ordinary snapshot RPC was incorrectly promoted to a live Stream")
	}
}

func TestV2RPCLegacyErrorsPreserveStableMachineCodes(t *testing.T) {
	tests := map[remotev1.RpcErrorCode]string{
		remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT:       "invalid_argument",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND:              "not_found",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN:              "forbidden",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT:      "revision_conflict",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_IDEMPOTENCY_CONFLICT:   "idempotency_conflict",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY:                   "busy",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_CANCELLED:              "cancelled",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL:               "internal",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH:       "project_mismatch",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE: "capability_unavailable",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_DEADLINE_EXCEEDED:      "deadline_exceeded",
		remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED:     "resource_exhausted",
	}
	for code, want := range tests {
		if got := v2RPCErrorCodeName(code); got != want {
			t.Errorf("v2RPCErrorCodeName(%v) = %q, want %q", code, got, want)
		}
	}
	request := &remotev2.RpcRequest{OperationId: uuid.NewString(), AttemptId: uuid.NewString()}
	legacy := &remotev1.RpcEnvelope{Message: &remotev1.RpcEnvelope_Response{Response: &remotev1.RpcResponse{
		Error: &remotev1.RpcError{Code: remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND, SafeMessage: "resource not found"},
	}}}
	response := v2RPCResponseFromLegacy(request, legacy)
	if response.GetSafeErrorCode() != "not_found" {
		t.Fatalf("legacy NOT_FOUND safe code = %q", response.GetSafeErrorCode())
	}
	legacy.GetResponse().Error.SafeMessage = "LOG_EXPIRED"
	response = v2RPCResponseFromLegacy(request, legacy)
	if response.GetSafeErrorCode() != "LOG_EXPIRED" {
		t.Fatalf("legacy semantic safe code = %q", response.GetSafeErrorCode())
	}
}

func TestV2DurableAIGenerationSurvivesTransportStreamCancellation(t *testing.T) {
	providerStarted := make(chan struct{})
	providerRelease := make(chan struct{})
	providerCancelled := make(chan struct{}, 1)
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		startedOnce.Do(func() { close(providerStarted) })
		select {
		case <-request.Context().Done():
			providerCancelled <- struct{}{}
			return
		case <-providerRelease:
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"continued\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(server.Close)

	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.AIConfigs["default"] = aiConfig{
		ID: "default", Name: "Test", Provider: "openai-compatible", BaseURL: server.URL,
		Model: "model-a", Enabled: true, Revision: 1,
	}
	state.mu.Unlock()
	dispatch := dispatcher{state: state, scope: "remote.peer.ai.chat", now: func() time.Time { return time.Now().UTC() }}
	created := dispatchJSON(t, dispatch, "conversation.create", `{"title":"Durable transport recovery"}`)
	conversationID := created["id"].(string)

	streamContext, closeTransportStream := context.WithCancel(context.Background())
	dispatchContext := v2RPCDispatchContext(streamContext, "conversation.send")
	completed := make(chan error, 1)
	go func() {
		_, _, callErr := dispatch.callConversationSend(dispatchContext, rpcInput{
			"conversationId": conversationID,
			"messageId":      uuid.NewString(),
			"content":        "keep running after reconnect",
		})
		completed <- callErr
	}()

	select {
	case <-providerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("OpenAI request did not start")
	}
	closeTransportStream()
	select {
	case <-providerCancelled:
		t.Fatal("transport Stream cancellation stopped the durable AI provider request")
	case <-time.After(100 * time.Millisecond):
	}
	close(providerRelease)
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("conversation.send after transport cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("durable AI generation did not complete")
	}

	stored := dispatchJSON(t, dispatch, "conversation.get", `{"conversationId":"`+conversationID+`"}`)
	conversation := stored["conversation"].(map[string]any)
	messages := stored["messages"].([]any)
	if conversation["state"] != "idle" || len(messages) != 2 || messages[1].(map[string]any)["content"] != "continued" || messages[1].(map[string]any)["status"] != "complete" {
		t.Fatalf("recovered conversation = %#v messages=%#v", conversation, messages)
	}
}

func TestV2OnlyDurableGenerationMethodsDetachFromStreamLifecycle(t *testing.T) {
	for _, method := range []string{"conversation.send", "conversation.chat.send", "chat.send", "conversation.regenerate"} {
		t.Run(method, func(t *testing.T) {
			streamContext, closeStream := context.WithCancel(context.Background())
			generationContext, release := contextWithoutPeerTransportCancellation(v2RPCDispatchContext(streamContext, method))
			defer release()
			closeStream()
			if err := generationContext.Err(); err != nil {
				t.Fatalf("durable generation inherited Stream cancellation: %v", err)
			}
		})
	}
	for _, method := range []string{"conversation.generation.attach", "terminal.attach", "conversation.get"} {
		t.Run(method, func(t *testing.T) {
			streamContext, closeStream := context.WithCancel(context.Background())
			operationContext, release := contextWithoutPeerTransportCancellation(v2RPCDispatchContext(streamContext, method))
			defer release()
			closeStream()
			if !errors.Is(operationContext.Err(), context.Canceled) {
				t.Fatalf("ordinary operation ignored Stream cancellation: %v", operationContext.Err())
			}
		})
	}
}
