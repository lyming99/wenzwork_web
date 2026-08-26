package remotecontrol

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

func TestHostProtocolDiagnosticIsStableAndContentFree(t *testing.T) {
	var captured HostProtocolDiagnostic
	issuer := &BrowserPeerTicketIssuer{
		signer:             remoteauth.Issuer{PrivateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))},
		now:                func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
		protocolDiagnostic: func(value HostProtocolDiagnostic) { captured = value },
	}
	targetID, projectID, sessionID := uuid.New(), uuid.New(), uuid.New()
	input := PeerIssueInput{
		TargetDeviceID: targetID, ProjectID: &projectID, Scope: "unreviewed-secret-scope", IdempotencyKey: "request-secret-value",
	}
	issuer.recordHostProtocolDiagnostic(input, PeerSession{SessionID: sessionID}, ErrProtocolVersion, 2, 25*time.Millisecond)
	if captured.Result != "failed" || captured.Reason != "relay_protocol_version_invalid" || captured.Scope != "unknown" ||
		captured.ObservedProtocol != 2 || captured.TargetHash == "" || captured.ProjectHash == "" || captured.RequestHash == "" ||
		captured.SessionHash == "" || captured.DurationBucket != "10_to_100ms" {
		t.Fatalf("diagnostic = %#v", captured)
	}
	encoded, err := json.Marshal(captured)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{targetID.String(), projectID.String(), sessionID.String(), input.IdempotencyKey, input.Scope, "ticket"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("diagnostic leaked %q", forbidden)
		}
	}
}
