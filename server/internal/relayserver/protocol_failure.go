package relayserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

var relayCorrelationSalt = func() [32]byte {
	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return sha256.Sum256([]byte("wenzwork-relay-correlation-fallback-v1"))
	}
	return salt
}()

const (
	relayReasonAuthorizationRequired  = "authorization_required"
	relayReasonSubprotocolMismatch    = "relay_subprotocol_mismatch"
	relayReasonProtocolVersionInvalid = "relay_protocol_version_invalid"
	relayReasonHandshakeFrameInvalid  = "relay_handshake_frame_invalid"
	relayReasonEnvelopeInvalid        = "relay_envelope_invalid"
	relayReasonFrameKindUnknown       = "relay_frame_kind_unknown"
	relayReasonFrameSizeInvalid       = "relay_frame_size_invalid"
	relayReasonPeerBindingInvalid     = "peer_binding_invalid"
	relayReasonPeerEpochStale         = "peer_epoch_stale"
	relayReasonAuthenticationFailed   = "relay_authentication_failed"
	relayReasonRouteUnavailable       = "relay_route_unavailable"
	relayReasonBackendUnavailable     = "relay_backend_unavailable"
	relayReasonRateLimited            = "relay_rate_limited"
	relayReasonForwardingUnavailable  = "relay_forwarding_unavailable"
)

type RelayProtocolFailure struct {
	Stage           string
	Reason          string
	FaultLevel      string
	Role            string
	ProtocolVersion uint32
	FrameSizeBucket string
	CorrelationID   string
}

// RelayConnectionLifecycle is a content-free connection record intended for
// the Relay process log.  It deliberately excludes ticket contents, endpoint
// URLs, device identifiers and frame bodies. CorrelationID is a Relay-local
// HMAC, so records for one carrier can be grouped without exposing its owner.
type RelayConnectionLifecycle struct {
	Event                          string
	Reason                         string
	Role                           string
	ConnectionEpoch                uint64
	CorrelationID                  string
	HandshakeMilliseconds          int64
	ConnectionLifetimeMilliseconds int64
	HeartbeatSeconds               uint32
	FramesInWindow                 int
	BytesInWindow                  int
	MaxFramesPerSecond             int
	MaxBytesPerSecond              int
}

type relayProtocolError struct {
	reason string
	cause  error
}

func (failure *relayProtocolError) Error() string { return failure.reason }
func (failure *relayProtocolError) Unwrap() error { return failure.cause }

func newRelayProtocolError(reason string, cause error) error {
	return &relayProtocolError{reason: reason, cause: cause}
}

func relayProtocolReason(err error) (string, bool) {
	var failure *relayProtocolError
	if !errors.As(err, &failure) {
		return "", false
	}
	return failure.reason, true
}

func relayFrameSizeBucket(size int) string {
	switch {
	case size <= 4<<10:
		return "at_or_below_4KiB"
	case size <= 60<<10:
		return "4_to_60KiB"
	case size <= 1<<20:
		return "60KiB_to_1MiB"
	default:
		return "over_1MiB"
	}
}

func relayCorrelationID(value string) string {
	if value == "" {
		return ""
	}
	digest := hmac.New(sha256.New, relayCorrelationSalt[:])
	_, _ = digest.Write([]byte("relay-correlation\x00" + value))
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil)[:9])
}
