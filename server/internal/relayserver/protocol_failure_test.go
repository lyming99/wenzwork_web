package relayserver

import (
	"errors"
	"testing"
)

func TestRelayProtocolFailureClassificationIsStableAndContentFree(t *testing.T) {
	cause := errors.New("payload contains secret text")
	err := newRelayProtocolError(relayReasonEnvelopeInvalid, cause)
	reason, ok := relayProtocolReason(err)
	if !ok || reason != relayReasonEnvelopeInvalid || !errors.Is(err, cause) || err.Error() != relayReasonEnvelopeInvalid {
		t.Fatalf("classified error = %q, reason=%q, classified=%v", err, reason, ok)
	}

	first := relayCorrelationID("device-or-session-identifier")
	second := relayCorrelationID("device-or-session-identifier")
	if first == "" || first != second || first == "device-or-session-identifier" {
		t.Fatalf("unsafe or unstable correlation id %q", first)
	}
	for size, want := range map[int]string{
		4096: "at_or_below_4KiB", 4097: "4_to_60KiB", 60 << 10: "4_to_60KiB", (60 << 10) + 1: "60KiB_to_1MiB", (1 << 20) + 1: "over_1MiB",
	} {
		if got := relayFrameSizeBucket(size); got != want {
			t.Fatalf("bucket(%d) = %q, want %q", size, got, want)
		}
	}
}

func TestRelayProtocolFailureHookReceivesOnlySafeDimensions(t *testing.T) {
	var captured RelayProtocolFailure
	handler := &Handler{ProtocolFailure: func(failure RelayProtocolFailure) { captured = failure }}
	handler.recordProtocolFailure("relayEnvelope", relayReasonFrameKindUnknown, "connection", "peer", 1, 8192, "raw-device-id")
	if captured.Stage != "relayEnvelope" || captured.Reason != relayReasonFrameKindUnknown ||
		captured.FaultLevel != "connection" || captured.Role != "peer" || captured.ProtocolVersion != 1 ||
		captured.FrameSizeBucket != "4_to_60KiB" || captured.CorrelationID == "" || captured.CorrelationID == "raw-device-id" {
		t.Fatalf("captured diagnostic = %#v", captured)
	}
}
