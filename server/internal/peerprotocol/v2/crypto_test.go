package peerv2

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"strconv"
	"testing"
	"time"
)

func testHandshakeBinding() HandshakeBinding {
	return HandshakeBinding{
		GrantID:               "grant-7f8b",
		LinkID:                "link-7f8b",
		ClientID:              "controller-7f8b",
		DeviceID:              "device-7f8b",
		RelayNodeID:           "relay-node-7f8b",
		RelayCellID:           "relay-cell-7f8b",
		TargetConnectionEpoch: 9,
		ClientIdentityVersion: 2,
		DeviceIdentityVersion: 4,
		ClientEphemeralPublic: bytes.Repeat([]byte{0x11}, X25519PublicKeySize),
		DeviceEphemeralPublic: bytes.Repeat([]byte{0x22}, X25519PublicKeySize),
		ClientChallenge:       bytes.Repeat([]byte{0x33}, 32),
		DeviceChallenge:       bytes.Repeat([]byte{0x44}, 32),
		ExpiresAtUnixMilli:    1_800_000_000_000,
	}
}

func TestHandshakeSignaturesAndRootDerivation(t *testing.T) {
	binding := testHandshakeBinding()
	clientPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x01}, ed25519.SeedSize))
	devicePrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x02}, ed25519.SeedSize))
	initBinding := binding
	initBinding.DeviceIdentityVersion = 0
	initBinding.DeviceEphemeralPublic = nil
	initBinding.DeviceChallenge = nil
	initSignature, err := SignLinkInit(clientPrivate, initBinding)
	if err != nil {
		t.Fatalf("SignLinkInit() error = %v", err)
	}
	if err := VerifyLinkInit(clientPrivate.Public().(ed25519.PublicKey), initBinding, initSignature); err != nil {
		t.Fatalf("VerifyLinkInit() error = %v", err)
	}
	if err := VerifyLinkInit(clientPrivate.Public().(ed25519.PublicKey), binding, initSignature); !errors.Is(err, ErrInvalidHandshake) {
		t.Fatalf("VerifyLinkInit() with altered transcript error = %v, want ErrInvalidHandshake", err)
	}
	acceptSignature, err := SignLinkAccept(devicePrivate, binding)
	if err != nil {
		t.Fatalf("SignLinkAccept() error = %v", err)
	}
	if err := VerifyLinkAccept(devicePrivate.Public().(ed25519.PublicKey), binding, acceptSignature); err != nil {
		t.Fatalf("VerifyLinkAccept() error = %v", err)
	}

	clientPrivateECDH, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	devicePrivateECDH, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sharedOne, err := X25519SharedSecret(clientPrivateECDH, devicePrivateECDH.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("X25519SharedSecret(client) error = %v", err)
	}
	sharedTwo, err := X25519SharedSecret(devicePrivateECDH, clientPrivateECDH.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("X25519SharedSecret(device) error = %v", err)
	}
	if !bytes.Equal(sharedOne, sharedTwo) {
		t.Fatal("X25519 shared secrets differ")
	}
	rootOne, err := DeriveRootKey(sharedOne, binding)
	if err != nil {
		t.Fatalf("DeriveRootKey(client) error = %v", err)
	}
	rootTwo, err := DeriveRootKey(sharedTwo, binding)
	if err != nil {
		t.Fatalf("DeriveRootKey(device) error = %v", err)
	}
	if !bytes.Equal(rootOne, rootTwo) {
		t.Fatal("root keys differ")
	}
	changedBinding := binding
	changedBinding.LinkID = "other-link"
	changedRoot, err := DeriveRootKey(sharedOne, changedBinding)
	if err != nil {
		t.Fatalf("DeriveRootKey(changed) error = %v", err)
	}
	if bytes.Equal(rootOne, changedRoot) {
		t.Fatal("root key did not bind the link ID")
	}
}

func TestRecordCipherBindsAllMetadataAndNonce(t *testing.T) {
	rootKey := bytes.Repeat([]byte{0x61}, RootKeySize)
	streamKey, err := DeriveStreamKey(rootKey, "link", 1, DirectionClientToDevice, "channel", "stream")
	if err != nil {
		t.Fatalf("DeriveStreamKey() error = %v", err)
	}
	metadata := RecordMetadata{
		LinkID: "link", ChannelID: "channel", StreamID: "stream", KeyID: 1,
		Direction: DirectionClientToDevice, FrameType: FrameRPCRequest, StreamSequence: 1,
	}
	ciphertext, err := Seal(streamKey, []byte("sensitive request"), metadata)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	plaintext, err := Open(streamKey, ciphertext, metadata)
	if err != nil || string(plaintext) != "sensitive request" {
		t.Fatalf("Open() = %q, %v", plaintext, err)
	}
	for name, altered := range map[string]RecordMetadata{
		"link":      func() RecordMetadata { value := metadata; value.LinkID = "other"; return value }(),
		"channel":   func() RecordMetadata { value := metadata; value.ChannelID = "other"; return value }(),
		"stream":    func() RecordMetadata { value := metadata; value.StreamID = "other"; return value }(),
		"key":       func() RecordMetadata { value := metadata; value.KeyID = 2; return value }(),
		"direction": func() RecordMetadata { value := metadata; value.Direction = DirectionDeviceToClient; return value }(),
		"frame":     func() RecordMetadata { value := metadata; value.FrameType = FrameRPCResponse; return value }(),
		"sequence":  func() RecordMetadata { value := metadata; value.StreamSequence = 2; return value }(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Open(streamKey, ciphertext, altered); !errors.Is(err, ErrAuthentication) && !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("Open() altered %s error = %v, want authenticated rejection", name, err)
			}
		})
	}
	firstNonce, err := Nonce(streamKey, metadata)
	if err != nil {
		t.Fatal(err)
	}
	secondMetadata := metadata
	secondMetadata.StreamSequence = 2
	secondNonce, err := Nonce(streamKey, secondMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstNonce, secondNonce) || !bytes.Equal(firstNonce[:16], secondNonce[:16]) {
		t.Fatal("nonce did not retain a key-domain prefix plus unique sequence suffix")
	}
}

func TestRenewableLeaseFrameTypesAreAuthenticatedAndAppendOnly(t *testing.T) {
	key := bytes.Repeat([]byte{0x6a}, RootKeySize)
	for _, frameType := range []FrameType{FrameLinkLeaseRenew, FrameLinkLeaseRenewed} {
		metadata := RecordMetadata{
			LinkID: "link", ChannelID: "v2-control", StreamID: "v2-control", KeyID: 1,
			Direction: DirectionClientToDevice, FrameType: frameType, StreamSequence: 1,
		}
		if _, err := AssociatedData(metadata); err != nil {
			t.Fatalf("AssociatedData(%d) error = %v", frameType, err)
		}
	}
	invalid := RecordMetadata{
		LinkID: "link", ChannelID: "v2-control", StreamID: "v2-control", KeyID: 1,
		Direction: DirectionClientToDevice, FrameType: FrameLinkLeaseRenewed + 1, StreamSequence: 1,
	}
	if _, err := Seal(key, []byte("future frame"), invalid); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("Seal(future frame) error = %v, want ErrInvalidMetadata", err)
	}
}

func TestSequencerAcceptsBoundedOutOfOrderOldFramesOnlyOnce(t *testing.T) {
	sequencer := NewSequencer(4)
	for _, sequence := range []uint64{3, 1, 2, 5, 4} {
		if err := sequencer.AcceptInbound(RecordMetadata{
			LinkID: "link", ChannelID: "channel", StreamID: "stream", KeyID: 1,
			Direction: DirectionDeviceToClient, FrameType: FrameRPCEvent, StreamSequence: sequence,
		}); err != nil {
			t.Fatalf("AcceptInbound(%d) error = %v", sequence, err)
		}
	}
	if err := sequencer.AcceptInbound(RecordMetadata{
		LinkID: "link", ChannelID: "channel", StreamID: "stream", KeyID: 1,
		Direction: DirectionDeviceToClient, FrameType: FrameRPCEvent, StreamSequence: 3,
	}); !errors.Is(err, ErrSequence) {
		t.Fatalf("duplicate frame error = %v, want ErrSequence", err)
	}
	if err := sequencer.AcceptInbound(RecordMetadata{
		LinkID: "link", ChannelID: "channel", StreamID: "stream", KeyID: 1,
		Direction: DirectionDeviceToClient, FrameType: FrameRPCEvent, StreamSequence: 8,
	}); err != nil {
		t.Fatalf("AcceptInbound(8) error = %v", err)
	}
	if err := sequencer.AcceptInbound(RecordMetadata{
		LinkID: "link", ChannelID: "channel", StreamID: "stream", KeyID: 1,
		Direction: DirectionDeviceToClient, FrameType: FrameRPCEvent, StreamSequence: 1,
	}); !errors.Is(err, ErrSequence) {
		t.Fatalf("expired replay-window frame error = %v, want ErrSequence", err)
	}
}

func TestSequencerClosedStreamRejectsReplayUntilKeyRetirement(t *testing.T) {
	sequencer := NewSequencer(4)
	if err := sequencer.OpenStream("stream-lifecycle"); err != nil {
		t.Fatal(err)
	}
	if err := sequencer.OpenStream("stream-lifecycle"); !errors.Is(err, ErrStreamReuse) {
		t.Fatalf("active Stream reuse error = %v, want ErrStreamReuse", err)
	}
	if _, err := sequencer.Next(1, DirectionClientToDevice, "stream-lifecycle"); err != nil {
		t.Fatal(err)
	}
	metadata := RecordMetadata{
		LinkID: "link", ChannelID: "channel", StreamID: "stream-lifecycle", KeyID: 1,
		Direction: DirectionDeviceToClient, FrameType: FrameRPCResponse, StreamSequence: 1,
	}
	if err := sequencer.AcceptInbound(metadata); err != nil {
		t.Fatal(err)
	}
	if err := sequencer.CloseStream("stream-lifecycle", time.Now()); err != nil {
		t.Fatal(err)
	}
	stats := sequencer.Stats()
	if stats.OutboundEntries != 0 || stats.InboundEntries != 0 || stats.Tombstones != 2 || stats.MaximumClosedSequence != 1 {
		t.Fatalf("closed stats = %+v", stats)
	}
	if err := sequencer.AcceptInbound(metadata); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("closed replay error = %v", err)
	}
	if err := sequencer.OpenStream("stream-lifecycle"); !errors.Is(err, ErrStreamReuse) {
		t.Fatalf("closed Stream reuse error = %v", err)
	}
	sequencer.RetireKey(1)
	if stats := sequencer.Stats(); stats.Tombstones != 0 || stats.KeyCount != 0 || stats.UsedStreamIDs != 1 {
		t.Fatalf("retired stats = %+v", stats)
	}
	if err := sequencer.OpenStream("stream-lifecycle"); !errors.Is(err, ErrStreamReuse) {
		t.Fatalf("retired Stream reuse error = %v, want ErrStreamReuse", err)
	}
}

func TestSequencerOneHundredThousandStreamLifecyclesStayBounded(t *testing.T) {
	sequencer := NewSequencerWithLimits(4096, 256, DefaultSequencerActiveLimit)
	keyID := uint64(1)
	maximumTombstones := 0
	closedAt := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 100000; index++ {
		streamID := "stream-" + strconv.Itoa(index)
		if err := sequencer.OpenStream(streamID); err != nil {
			t.Fatalf("OpenStream(%d): %v", index, err)
		}
		if _, err := sequencer.Next(keyID, DirectionDeviceToClient, streamID); err != nil {
			t.Fatalf("Next(%d): %v", index, err)
		}
		if err := sequencer.CloseStream(streamID, closedAt); err != nil {
			t.Fatalf("CloseStream(%d): %v", index, err)
		}
		if count := sequencer.Stats().Tombstones; count > maximumTombstones {
			maximumTombstones = count
		}
		if (index+1)%100 == 0 {
			sequencer.RetireKey(keyID)
			keyID++
		}
	}
	sequencer.RetireKey(keyID)
	stats := sequencer.Stats()
	if maximumTombstones > 200 || stats.OutboundEntries != 0 || stats.InboundEntries != 0 || stats.Tombstones != 0 || stats.ActiveStreams != 0 ||
		stats.UsedStreamIDs != 100000 || stats.UsedStreamIDs > stats.MaximumStreamsPerLink {
		t.Fatalf("bounded lifecycle max=%d stats=%+v", maximumTombstones, stats)
	}
}

func TestSequencerStreamIDsStayUniqueUntilLinkReset(t *testing.T) {
	sequencer := NewSequencerWithResourceLimits(4, 256, 8, 2)
	for index, streamID := range []string{"stream-one", "stream-two"} {
		if err := sequencer.OpenStream(streamID); err != nil {
			t.Fatal(err)
		}
		if _, err := sequencer.Next(uint64(index+1), DirectionClientToDevice, streamID); err != nil {
			t.Fatal(err)
		}
		if err := sequencer.CloseStream(streamID, time.Now()); err != nil {
			t.Fatal(err)
		}
		sequencer.RetireKey(uint64(index + 1))
		if err := sequencer.OpenStream(streamID); !errors.Is(err, ErrStreamReuse) {
			t.Fatalf("OpenStream(%q) after Key retirement = %v, want ErrStreamReuse", streamID, err)
		}
	}
	if err := sequencer.OpenStream("stream-three"); !errors.Is(err, ErrSequenceLimit) {
		t.Fatalf("OpenStream over Link limit = %v, want ErrSequenceLimit", err)
	}
	sequencer.Reset()
	if err := sequencer.OpenStream("stream-one"); err != nil {
		t.Fatalf("OpenStream after Link reset = %v", err)
	}
}

func TestSequencerTombstoneLimitFailsClosed(t *testing.T) {
	sequencer := NewSequencerWithLimits(4096, 2, 8)
	for _, streamID := range []string{"stream-one", "stream-two"} {
		if err := sequencer.OpenStream(streamID); err != nil {
			t.Fatal(err)
		}
		if _, err := sequencer.Next(1, DirectionDeviceToClient, streamID); err != nil {
			t.Fatal(err)
		}
		if err := sequencer.CloseStream(streamID, time.Now()); streamID == "stream-one" && err != nil {
			t.Fatal(err)
		} else if streamID == "stream-two" && !errors.Is(err, ErrSequenceLimit) {
			t.Fatalf("hard-limit error = %v", err)
		}
	}
	stats := sequencer.Stats()
	if stats.Tombstones != 2 || stats.ActiveStreams != 1 || stats.UsedStreamIDs != 2 {
		t.Fatalf("hard-limit stats = %+v", stats)
	}
}

func TestRekeyIsIdempotentAndRetainsPreviousGeneration(t *testing.T) {
	rootKey := bytes.Repeat([]byte{0x77}, RootKeySize)
	initiator, err := NewLinkState("link", rootKey)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := NewLinkState("link", rootKey)
	if err != nil {
		t.Fatal(err)
	}
	initiatorIdentity := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x01}, ed25519.SeedSize))
	responderIdentity := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x02}, ed25519.SeedSize))
	init, err := initiator.InitiateRekey(initiatorIdentity, bytes.NewReader(bytes.Repeat([]byte{0x90}, 50)))
	if err != nil {
		t.Fatalf("InitiateRekey() error = %v", err)
	}
	retriedInit, err := initiator.InitiateRekey(initiatorIdentity, bytes.NewReader(bytes.Repeat([]byte{0x91}, 50)))
	if err != nil || !bytes.Equal(init.IdentitySignature, retriedInit.IdentitySignature) || !bytes.Equal(init.EphemeralPublic, retriedInit.EphemeralPublic) {
		t.Fatalf("idempotent InitiateRekey() = %#v, %v", retriedInit, err)
	}
	ack, err := responder.ReceiveRekeyInit(*init, initiatorIdentity.Public().(ed25519.PublicKey), responderIdentity, bytes.NewReader(bytes.Repeat([]byte{0xa0}, 32)))
	if err != nil {
		t.Fatalf("ReceiveRekeyInit() error = %v", err)
	}
	retriedAck, err := responder.ReceiveRekeyInit(*init, initiatorIdentity.Public().(ed25519.PublicKey), responderIdentity, bytes.NewReader(bytes.Repeat([]byte{0xa1}, 32)))
	if err != nil || !bytes.Equal(ack.IdentitySignature, retriedAck.IdentitySignature) || !bytes.Equal(ack.EphemeralPublic, retriedAck.EphemeralPublic) {
		t.Fatalf("idempotent ReceiveRekeyInit() = %#v, %v", retriedAck, err)
	}
	if err := initiator.ReceiveRekeyAck(*ack, responderIdentity.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("ReceiveRekeyAck() error = %v", err)
	}
	commit, err := initiator.CommitRekey([]StreamBoundary{{StreamID: "rpc", NextSequence: 7}, {StreamID: "file", NextSequence: 11}})
	if err != nil {
		t.Fatalf("CommitRekey() error = %v", err)
	}
	if err := responder.ReceiveRekeyCommit(*commit); err != nil {
		t.Fatalf("ReceiveRekeyCommit() error = %v", err)
	}
	if initiator.ActiveKeyID() != 2 || responder.ActiveKeyID() != 2 {
		t.Fatalf("active key IDs = %d, %d, want 2", initiator.ActiveKeyID(), responder.ActiveKeyID())
	}
	initiatorRoot, err := initiator.RootKey(2)
	if err != nil {
		t.Fatal(err)
	}
	responderRoot, err := responder.RootKey(2)
	if err != nil || !bytes.Equal(initiatorRoot, responderRoot) {
		t.Fatalf("new root keys differ: %v", err)
	}
	if _, err := initiator.RootKey(1); err != nil {
		t.Fatalf("previous root was not retained: %v", err)
	}
	if !initiator.RetireKey(1) {
		t.Fatal("RetireKey(previous) = false, want true")
	}
	if _, err := initiator.RootKey(1); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("retired root lookup error = %v, want ErrKeyUnavailable", err)
	}
	initiator.Close()
	if _, err := initiator.RootKey(2); !errors.Is(err, ErrLinkClosed) {
		t.Fatalf("closed root lookup error = %v, want ErrLinkClosed", err)
	}
}

func TestRekeyCompletedControlFramesRemainIdempotent(t *testing.T) {
	rootKey := bytes.Repeat([]byte{0x33}, RootKeySize)
	initiator, err := NewLinkState("link", rootKey)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := NewLinkState("link", rootKey)
	if err != nil {
		t.Fatal(err)
	}
	initiatorIdentity := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x11}, ed25519.SeedSize))
	responderIdentity := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x22}, ed25519.SeedSize))
	init, err := initiator.InitiateRekey(initiatorIdentity, bytes.NewReader(bytes.Repeat([]byte{0x41}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	ack, err := responder.ReceiveRekeyInit(*init, initiatorIdentity.Public().(ed25519.PublicKey), responderIdentity, bytes.NewReader(bytes.Repeat([]byte{0x42}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	if err := initiator.ReceiveRekeyAck(*ack, responderIdentity.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	commit, err := initiator.CommitRekey([]StreamBoundary{{StreamID: "rpc", NextSequence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := responder.ReceiveRekeyCommit(*commit); err != nil {
		t.Fatal(err)
	}
	retry, err := initiator.LastRekeyRetry()
	if err != nil || retry == nil || !retry.Initiator || !sameRekeyCommit(retry.Commit, commit) {
		t.Fatalf("LastRekeyRetry() = %#v, %v", retry, err)
	}
	// ACK/INIT/COMMIT retransmissions after activation are all harmless.
	if err := initiator.ReceiveRekeyAck(*ack, responderIdentity.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("duplicate ACK = %v", err)
	}
	retriedCommit, err := initiator.CommitRekey([]StreamBoundary{{StreamID: "different", NextSequence: 99}})
	if err != nil || !sameRekeyCommit(commit, retriedCommit) {
		t.Fatalf("duplicate commit = %#v, %v", retriedCommit, err)
	}
	if _, err := responder.ReceiveRekeyInit(*init, initiatorIdentity.Public().(ed25519.PublicKey), responderIdentity, bytes.NewReader(bytes.Repeat([]byte{0x99}, 64))); err != nil {
		t.Fatalf("duplicate INIT = %v", err)
	}
	if err := responder.ReceiveRekeyCommit(*commit); err != nil {
		t.Fatalf("duplicate COMMIT = %v", err)
	}
	bad := cloneRekeyCommit(commit)
	bad.Boundaries[0].NextSequence++
	if err := responder.ReceiveRekeyCommit(*bad); !errors.Is(err, ErrRekeyConflict) {
		t.Fatalf("conflicting COMMIT = %v, want ErrRekeyConflict", err)
	}
}

func TestSequencerRequestsRolloverBeforeHardLimit(t *testing.T) {
	sequencer := NewSequencerWithResourceLimits(32, 8, 8, 4)
	for index := 0; index < 2; index++ {
		streamID := "stream-" + strconv.Itoa(index)
		if err := sequencer.OpenStream(streamID); err != nil {
			t.Fatal(err)
		}
		if err := sequencer.CloseStream(streamID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if sequencer.ShouldRollover() {
		t.Fatal("Sequencer requested rollover below the 75 percent threshold")
	}
	if err := sequencer.OpenStream("stream-2"); err != nil {
		t.Fatal(err)
	}
	if !sequencer.ShouldRollover() {
		t.Fatal("Sequencer did not request rollover at the 75 percent threshold")
	}
}
