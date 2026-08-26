package peerv2

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

// TestRemoteV2GoldenVectors verifies the shared fixture also consumed by the
// generated-code clients.  Changing a transcript, nonce or cipher binding
// must update every implementation deliberately.
func TestRemoteV2GoldenVectors(t *testing.T) {
	binding := HandshakeBinding{
		GrantID:               "grant-v2-golden",
		LinkID:                "link-v2-golden",
		ClientID:              "client-v2-golden",
		DeviceID:              "device-v2-golden",
		RelayNodeID:           "relay-node-v2",
		RelayCellID:           "relay-cell-v2",
		TargetConnectionEpoch: 9,
		ClientIdentityVersion: 2,
		DeviceIdentityVersion: 4,
		ClientEphemeralPublic: bytes.Repeat([]byte{0x11}, X25519PublicKeySize),
		DeviceEphemeralPublic: bytes.Repeat([]byte{0x22}, X25519PublicKeySize),
		ClientChallenge:       bytes.Repeat([]byte{0x33}, 32),
		DeviceChallenge:       bytes.Repeat([]byte{0x44}, 32),
		ExpiresAtUnixMilli:    1_800_000_000_000,
	}
	initBinding := binding
	initBinding.DeviceIdentityVersion = 0
	initBinding.DeviceEphemeralPublic = nil
	initBinding.DeviceChallenge = nil
	shared := bytes.Repeat([]byte{0x55}, X25519PublicKeySize)
	root, err := DeriveRootKey(shared, binding)
	if err != nil {
		t.Fatal(err)
	}
	metadata := RecordMetadata{
		LinkID:         binding.LinkID,
		ChannelID:      "channel-v2-golden",
		StreamID:       "stream-v2-golden",
		KeyID:          1,
		Direction:      DirectionClientToDevice,
		FrameType:      FrameRPCRequest,
		StreamSequence: 7,
	}
	controlKey, err := DeriveControlKey(root, binding.LinkID, 1, DirectionClientToDevice)
	if err != nil {
		t.Fatal(err)
	}
	channelKey, err := DeriveChannelKey(root, binding.LinkID, 1, DirectionClientToDevice, metadata.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	streamKey, err := DeriveStreamKey(root, binding.LinkID, 1, DirectionClientToDevice, metadata.ChannelID, metadata.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("remote/v2 golden payload")
	ciphertext, err := Seal(streamKey, plaintext, metadata)
	if err != nil {
		t.Fatal(err)
	}
	initTranscript, err := CanonicalInitTranscript(initBinding)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := CanonicalHandshakeTranscript(binding)
	if err != nil {
		t.Fatal(err)
	}
	transcriptHash, err := TranscriptHash(binding)
	if err != nil {
		t.Fatal(err)
	}
	confirmation, err := LinkConfirmationMAC(root, binding)
	if err != nil {
		t.Fatal(err)
	}
	associatedData, err := AssociatedData(metadata)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := Nonce(streamKey, metadata)
	if err != nil {
		t.Fatal(err)
	}
	rekeyShared := bytes.Repeat([]byte{0x66}, X25519PublicKeySize)
	rekeyRoot, err := deriveRekeyRoot(root, rekeyShared, binding.LinkID, "rekey-v2-golden", 2)
	if err != nil {
		t.Fatal(err)
	}
	rekeyPublic := bytes.Repeat([]byte{0x77}, X25519PublicKeySize)
	carrierChallenge := bytes.Repeat([]byte{0x88}, 32)
	clientPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x01}, ed25519.SeedSize))
	devicePrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x02}, ed25519.SeedSize))
	initSignature, err := SignLinkInit(clientPrivate, initBinding)
	if err != nil {
		t.Fatal(err)
	}
	acceptSignature, err := SignLinkAccept(devicePrivate, binding)
	if err != nil {
		t.Fatal(err)
	}
	carrierProof := remoteauth.CarrierProof{GrantID: binding.GrantID, CarrierID: "carrier-v2-golden", CarrierEpoch: 3, Challenge: carrierChallenge}
	carrierSignature, err := remoteauth.SignCarrierProof(clientPrivate, carrierProof)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }
	terminalSessionID := "22222222-2222-4222-8222-222222222222"
	terminalInput := []byte("terminal-v4-中")
	terminalHelloFrame := &remotev2.TerminalStreamFrame{
		SessionId: terminalSessionID,
		Body: &remotev2.TerminalStreamFrame_Hello{Hello: &remotev2.TerminalStreamHello{
			AfterOutputSequence: 7, AfterInputSequence: 3, AfterResizeSequence: 2, OutputCreditBytes: 65536,
		}},
	}
	terminalInputFrame := &remotev2.TerminalStreamFrame{
		SessionId: terminalSessionID,
		Body: &remotev2.TerminalStreamFrame_Input{Input: &remotev2.TerminalInput{
			Sequence: 4, Data: terminalInput,
		}},
	}
	terminalHelloBytes, err := proto.Marshal(terminalHelloFrame)
	if err != nil {
		t.Fatal(err)
	}
	terminalInputBytes, err := proto.Marshal(terminalInputFrame)
	if err != nil {
		t.Fatal(err)
	}
	value := map[string]any{
		"version": 1,
		"binding": map[string]any{
			"grantId": binding.GrantID, "linkId": binding.LinkID, "clientId": binding.ClientID, "deviceId": binding.DeviceID,
			"relayNodeId": binding.RelayNodeID, "relayCellId": binding.RelayCellID, "targetConnectionEpoch": binding.TargetConnectionEpoch,
			"clientIdentityVersion": binding.ClientIdentityVersion, "deviceIdentityVersion": binding.DeviceIdentityVersion,
			"clientEphemeralPublic": encode(binding.ClientEphemeralPublic), "deviceEphemeralPublic": encode(binding.DeviceEphemeralPublic),
			"clientChallenge": encode(binding.ClientChallenge), "deviceChallenge": encode(binding.DeviceChallenge), "expiresAtUnixMilli": binding.ExpiresAtUnixMilli,
		},
		"sharedSecret": encode(shared),
		"record": map[string]any{
			"channelId": metadata.ChannelID, "streamId": metadata.StreamID, "keyId": metadata.KeyID,
			"direction": metadata.Direction, "frameType": metadata.FrameType, "streamSequence": metadata.StreamSequence, "plaintext": encode(plaintext),
		},
		"terminalStream": map[string]any{
			"sessionId": terminalSessionID,
			"hello": map[string]any{
				"afterOutputSequence": 7, "afterInputSequence": 3, "afterResizeSequence": 2,
				"outputCreditBytes": 65536, "base64Url": encode(terminalHelloBytes),
			},
			"input": map[string]any{
				"sequence": 4, "dataBase64Url": encode(terminalInput), "base64Url": encode(terminalInputBytes),
			},
		},
		"rekey":         map[string]any{"rekeyId": "rekey-v2-golden", "keyId": 2, "sharedSecret": encode(rekeyShared), "ephemeralPublic": encode(rekeyPublic)},
		"carrierProof":  map[string]any{"carrierId": carrierProof.CarrierID, "carrierEpoch": carrierProof.CarrierEpoch, "challenge": encode(carrierChallenge)},
		"identitySeeds": map[string]any{"client": encode(bytes.Repeat([]byte{0x01}, ed25519.SeedSize)), "device": encode(bytes.Repeat([]byte{0x02}, ed25519.SeedSize))},
		"sequencerLifecycle": map[string]any{
			"window": 4, "tombstoneLimit": 2, "activeLimit": 8, "maximumStreamsPerLink": 2, "keyId": 1,
			"linkId": "link-lifecycle", "channelId": "channel-lifecycle", "streamId": "stream-lifecycle", "secondStreamId": "stream-lifecycle-two", "thirdStreamId": "stream-lifecycle-three",
			"outboundSequences": []int{1, 2, 3}, "inboundSequences": []int{1, 3, 2}, "replayedInboundSequence": 2,
			"closedAtUnixMilli": int64(1_787_097_600_000),
			"expectedBeforeClose": map[string]any{
				"outboundEntries": 1, "inboundEntries": 1, "tombstones": 0, "activeStreams": 1, "usedStreamIds": 1, "keyCount": 1, "maximumClosedSequence": 0, "maximumStreamsPerLink": 2,
			},
			"expectedAfterClose": map[string]any{
				"outboundEntries": 0, "inboundEntries": 0, "tombstones": 2, "activeStreams": 0, "usedStreamIds": 1, "keyCount": 1, "maximumClosedSequence": 3, "maximumStreamsPerLink": 2,
			},
			"expectedHardLimit": map[string]any{
				"outboundEntries": 1, "inboundEntries": 0, "tombstones": 2, "activeStreams": 1, "usedStreamIds": 2, "keyCount": 1, "maximumClosedSequence": 1, "maximumStreamsPerLink": 2,
			},
			"expectedAfterRetire": map[string]any{
				"outboundEntries": 0, "inboundEntries": 0, "tombstones": 0, "activeStreams": 0, "usedStreamIds": 1, "keyCount": 0, "maximumClosedSequence": 0, "maximumStreamsPerLink": 2,
			},
			"expectedAfterReset": map[string]any{
				"outboundEntries": 0, "inboundEntries": 0, "tombstones": 0, "activeStreams": 1, "usedStreamIds": 1, "keyCount": 0, "maximumClosedSequence": 0, "maximumStreamsPerLink": 2,
			},
		},
		"expected": map[string]any{
			"initTranscript": encode(initTranscript), "handshakeTranscript": encode(transcript), "transcriptHash": encode(transcriptHash[:]),
			"rootKey": encode(root), "linkConfirmationMac": encode(confirmation), "controlKey": encode(controlKey), "channelKey": encode(channelKey),
			"streamKey": encode(streamKey), "associatedData": encode(associatedData), "nonce": encode(nonce), "ciphertext": encode(ciphertext),
			"rekeyRoot": encode(rekeyRoot), "canonicalRekeyInit": encode(canonicalRekey("init", binding.LinkID, "rekey-v2-golden", 2, rekeyPublic)),
			"initSignature": encode(initSignature), "acceptSignature": encode(acceptSignature), "carrierProofSignature": encode(carrierSignature),
		},
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "api", "remote", "v2", "golden_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remote/v2 golden vectors changed unexpectedly\n got: %s\nwant: %s", encoded, fixture)
	}
}

type sequencerGoldenStats struct {
	OutboundEntries       int    `json:"outboundEntries"`
	InboundEntries        int    `json:"inboundEntries"`
	Tombstones            int    `json:"tombstones"`
	ActiveStreams         int    `json:"activeStreams"`
	UsedStreamIDs         int    `json:"usedStreamIds"`
	KeyCount              int    `json:"keyCount"`
	MaximumClosedSequence uint64 `json:"maximumClosedSequence"`
	MaximumStreamsPerLink int    `json:"maximumStreamsPerLink"`
}

func TestSequencerLifecycleUsesSharedGoldenVectors(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "api", "remote", "v2", "golden_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Sequencer struct {
			Window                  uint64               `json:"window"`
			TombstoneLimit          int                  `json:"tombstoneLimit"`
			ActiveLimit             int                  `json:"activeLimit"`
			MaximumStreamsPerLink   int                  `json:"maximumStreamsPerLink"`
			KeyID                   uint64               `json:"keyId"`
			LinkID                  string               `json:"linkId"`
			ChannelID               string               `json:"channelId"`
			StreamID                string               `json:"streamId"`
			SecondStreamID          string               `json:"secondStreamId"`
			ThirdStreamID           string               `json:"thirdStreamId"`
			OutboundSequences       []uint64             `json:"outboundSequences"`
			InboundSequences        []uint64             `json:"inboundSequences"`
			ReplayedInboundSequence uint64               `json:"replayedInboundSequence"`
			ClosedAtUnixMilli       int64                `json:"closedAtUnixMilli"`
			BeforeClose             sequencerGoldenStats `json:"expectedBeforeClose"`
			AfterClose              sequencerGoldenStats `json:"expectedAfterClose"`
			HardLimit               sequencerGoldenStats `json:"expectedHardLimit"`
			AfterRetire             sequencerGoldenStats `json:"expectedAfterRetire"`
			AfterReset              sequencerGoldenStats `json:"expectedAfterReset"`
		} `json:"sequencerLifecycle"`
	}
	if err := json.Unmarshal(fixture, &value); err != nil {
		t.Fatal(err)
	}
	vector := value.Sequencer
	sequencer := NewSequencerWithResourceLimits(vector.Window, vector.TombstoneLimit, vector.ActiveLimit, vector.MaximumStreamsPerLink)
	if err := sequencer.CloseStream(vector.ThirdStreamID, time.UnixMilli(vector.ClosedAtUnixMilli)); !errors.Is(err, ErrSequence) {
		t.Fatalf("unopened Stream close error = %v, want ErrSequence", err)
	}
	if err := sequencer.OpenStream(vector.StreamID); err != nil {
		t.Fatal(err)
	}
	if err := sequencer.OpenStream(vector.StreamID); !errors.Is(err, ErrStreamReuse) {
		t.Fatalf("active Stream reuse error = %v, want ErrStreamReuse", err)
	}
	for _, expected := range vector.OutboundSequences {
		if got, err := sequencer.Next(vector.KeyID, DirectionClientToDevice, vector.StreamID); err != nil || got != expected {
			t.Fatalf("outbound sequence = %d, %v; want %d", got, err, expected)
		}
	}
	metadata := RecordMetadata{
		LinkID: vector.LinkID, ChannelID: vector.ChannelID, StreamID: vector.StreamID, KeyID: vector.KeyID,
		Direction: DirectionDeviceToClient, FrameType: FrameRPCResponse,
	}
	for _, sequence := range vector.InboundSequences {
		metadata.StreamSequence = sequence
		if err := sequencer.AcceptInbound(metadata); err != nil {
			t.Fatalf("AcceptInbound(%d): %v", sequence, err)
		}
	}
	metadata.StreamSequence = vector.ReplayedInboundSequence
	if err := sequencer.AcceptInbound(metadata); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed inbound error = %v, want ErrReplay", err)
	}
	assertSequencerGoldenStats(t, sequencer.Stats(), vector.BeforeClose)
	if err := sequencer.CloseStream(vector.StreamID, time.UnixMilli(vector.ClosedAtUnixMilli)); err != nil {
		t.Fatal(err)
	}
	assertSequencerGoldenStats(t, sequencer.Stats(), vector.AfterClose)
	if err := sequencer.AcceptInbound(metadata); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("late closed frame error = %v, want ErrStreamClosed", err)
	}
	if err := sequencer.OpenStream(vector.StreamID); !errors.Is(err, ErrStreamReuse) {
		t.Fatalf("closed Stream reuse error = %v, want ErrStreamReuse", err)
	}
	sequencer.RetireKey(vector.KeyID)
	assertSequencerGoldenStats(t, sequencer.Stats(), vector.AfterRetire)
	if err := sequencer.CloseStream(vector.StreamID, time.UnixMilli(vector.ClosedAtUnixMilli)); err != nil {
		t.Fatalf("idempotent close after Key retirement: %v", err)
	}
	if err := sequencer.OpenStream(vector.StreamID); !errors.Is(err, ErrStreamReuse) {
		t.Fatalf("retired Stream reuse error = %v, want ErrStreamReuse", err)
	}
	sequencer.Reset()
	if err := sequencer.OpenStream(vector.StreamID); err != nil {
		t.Fatalf("Stream ID after Link reset: %v", err)
	}
	assertSequencerGoldenStats(t, sequencer.Stats(), vector.AfterReset)

	hard := NewSequencerWithResourceLimits(vector.Window, vector.TombstoneLimit, vector.ActiveLimit, vector.MaximumStreamsPerLink)
	for index, streamID := range []string{vector.StreamID, vector.SecondStreamID} {
		if err := hard.OpenStream(streamID); err != nil {
			t.Fatal(err)
		}
		if _, err := hard.Next(vector.KeyID, DirectionClientToDevice, streamID); err != nil {
			t.Fatal(err)
		}
		err := hard.CloseStream(streamID, time.UnixMilli(vector.ClosedAtUnixMilli))
		if index == 0 && err != nil {
			t.Fatal(err)
		}
		if index == 1 && !errors.Is(err, ErrSequenceLimit) {
			t.Fatalf("hard limit error = %v, want ErrSequenceLimit", err)
		}
	}
	if err := hard.OpenStream(vector.ThirdStreamID); !errors.Is(err, ErrSequenceLimit) {
		t.Fatalf("Link Stream capacity error = %v, want ErrSequenceLimit", err)
	}
	assertSequencerGoldenStats(t, hard.Stats(), vector.HardLimit)
}

func assertSequencerGoldenStats(t *testing.T, got SequencerStats, want sequencerGoldenStats) {
	t.Helper()
	if got.OutboundEntries != want.OutboundEntries || got.InboundEntries != want.InboundEntries || got.Tombstones != want.Tombstones ||
		got.ActiveStreams != want.ActiveStreams || got.UsedStreamIDs != want.UsedStreamIDs || got.KeyCount != want.KeyCount ||
		got.MaximumClosedSequence != want.MaximumClosedSequence || got.MaximumStreamsPerLink != want.MaximumStreamsPerLink {
		t.Fatalf("sequencer stats = %+v, want %+v", got, want)
	}
}
