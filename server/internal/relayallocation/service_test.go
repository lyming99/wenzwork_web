package relayallocation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

func TestMVPAllocationRejectsAnotherAuthenticatedDevice(t *testing.T) {
	service := &Service{region: "cn-dev"}
	_, err := service.Create(t.Context(), CreateInput{
		UserID: uuid.New(), SessionID: uuid.New(), DeviceID: uuid.New(), RemoteDeviceID: uuid.New(),
		IdempotencyKey: "allocation-123", ProtocolMin: 1, ProtocolMax: 1, ConnectionEpoch: 1,
	})
	if !errors.Is(err, ErrDeviceForbidden) {
		t.Fatalf("Create() error = %v, want ErrDeviceForbidden", err)
	}
}

func TestMVPAllocationValidatesEpochIdempotencyAndRefreshReasonBeforeDatabase(t *testing.T) {
	deviceID := uuid.New()
	service := &Service{region: "cn-dev"}
	for name, input := range map[string]CreateInput{
		"missing epoch": {
			UserID: uuid.New(), SessionID: uuid.New(), DeviceID: deviceID, RemoteDeviceID: deviceID,
			IdempotencyKey: "allocation-123", ProtocolMin: 1, ProtocolMax: 1,
		},
		"short idempotency key": {
			UserID: uuid.New(), SessionID: uuid.New(), DeviceID: deviceID, RemoteDeviceID: deviceID,
			IdempotencyKey: "short", ProtocolMin: 1, ProtocolMax: 1, ConnectionEpoch: 1,
		},
		"other region": {
			UserID: uuid.New(), SessionID: uuid.New(), DeviceID: deviceID, RemoteDeviceID: deviceID,
			IdempotencyKey: "allocation-123", ProtocolMin: 1, ProtocolMax: 1, ConnectionEpoch: 1, PreferredRegion: "other",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Create(t.Context(), input); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Create() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if _, err := service.Refresh(t.Context(), RefreshInput{
		UserID: uuid.New(), SessionID: uuid.New(), DeviceID: deviceID, AssignmentID: uuid.New(),
		IdempotencyKey: "refresh-1234", Reason: "arbitrary",
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Refresh() error = %v, want ErrInvalidRequest", err)
	}
}

func TestMVPConnectionTicketHasFixedClaimsAndFiveMinuteTTL(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	service := &Service{
		issuer:    remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "control-mvp-01", PrivateKey: privateKey},
		ticketTTL: 5 * time.Minute,
	}
	userID, deviceID, assignmentID, cellID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	thumbprint := remoteauth.PublicKeyThumbprint(publicKey)
	ticket, expiresAt, refreshAfter, err := service.signConnectionTicket(now, allocationAssignmentRow{
		ID: assignmentID, UserID: userID, AssignmentVersion: 7,
	}, allocationCredentialRow{
		DeviceID: deviceID, ProtocolMin: 1, ProtocolMax: 1, IdentityPublicKey: publicKey, PublicKeyThumbprint: thumbprint, GrantVersion: 3,
	}, []string{cellID.String()})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := (remoteauth.Verifier{
		Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"control-mvp-01": publicKey},
	}).Verify(ticket, "relay", now)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if claims.Subject != deviceID.String() || claims.UserID != userID.String() || claims.AssignmentID != assignmentID.String() ||
		claims.AssignmentVersion != 7 || claims.GrantVersion != 3 || claims.Confirmation != thumbprint ||
		!claims.HasScope("remote.connect") || len(claims.Scopes) != 1 ||
		len(claims.AllowedCellIDs) != 1 || claims.AllowedCellIDs[0] != cellID.String() ||
		time.Unix(claims.ExpiresAt, 0).Sub(time.Unix(claims.IssuedAt, 0)) != 5*time.Minute ||
		!expiresAt.Equal(now.Add(5*time.Minute)) || !refreshAfter.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("claims/times = %+v %s %s", claims, expiresAt, refreshAfter)
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(strings.Split(ticket, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		KeyID string `json:"kid"`
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.KeyID != "control-mvp-01" {
		t.Fatalf("ticket header = %s", headerBytes)
	}
}
