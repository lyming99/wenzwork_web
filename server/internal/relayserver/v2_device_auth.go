package relayserver

import (
	"context"
	"crypto/ed25519"
	"time"

	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

// V2TicketDeviceAuthenticator keeps device Carrier admission separate from a
// Client DeviceLinkGrant. The former is a device connection ticket; the latter
// is a reusable proof-bound Client authorization. Neither verifier accepts the
// other's claim type.
type V2TicketDeviceAuthenticator struct {
	Verifier TicketVerifier
	CellID   string
	NodeID   string
}

func (authenticator V2TicketDeviceAuthenticator) AuthenticateV2Device(_ context.Context, envelope *remotev2.CarrierEnvelope, hello *remotev2.CarrierHello, now time.Time) (V2DevicePrincipal, error) {
	if authenticator.Verifier == nil || authenticator.CellID == "" || authenticator.NodeID == "" || envelope == nil || hello == nil {
		return V2DevicePrincipal{}, ErrV2Route
	}
	claims, err := authenticator.Verifier.Verify(hello.GetDeviceConnectionTicket(), "relay", now)
	if err != nil || claims.Subject == "" || claims.Subject != hello.GetDeviceId() || hello.GetDeviceConnectionEpoch() != envelope.GetCarrierEpoch() {
		return V2DevicePrincipal{}, ErrV2Route
	}
	publicKey, err := remoteauth.DecodeIdentityPublicKey(claims.IdentityKey, claims.Confirmation)
	if err != nil || claims.ValidateConnection(claims.Subject, claims.UserID, authenticator.CellID, remoteauth.PublicKeyThumbprint(ed25519.PublicKey(publicKey)), claims.AssignmentVersion, claims.GrantVersion, 2) != nil {
		return V2DevicePrincipal{}, ErrV2Route
	}
	proof := remoteauth.CarrierProof{GrantID: claims.JWTID, CarrierID: envelope.GetCarrierId(), CarrierEpoch: envelope.GetCarrierEpoch(), Challenge: hello.GetClientChallenge()}
	if err := remoteauth.VerifyCarrierProof(ed25519.PublicKey(publicKey), proof, hello.GetDeviceProof()); err != nil {
		return V2DevicePrincipal{}, ErrV2Route
	}
	return V2DevicePrincipal{DeviceID: claims.Subject, UserID: claims.UserID, ConnectionEpoch: envelope.GetCarrierEpoch(), AssignmentVersion: claims.AssignmentVersion, GrantVersion: claims.GrantVersion}, nil
}
