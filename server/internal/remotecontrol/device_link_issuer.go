package remotecontrol

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"gorm.io/gorm"
)

const (
	// A zero default means the PoP-bound Grant has no periodic renewal
	// deadline. Explicit non-zero TTLs remain supported for compatibility and
	// tightly constrained deployments.
	DefaultDeviceLinkGrantTTL time.Duration = 0
	MaximumDeviceLinkGrantTTL               = 15 * time.Minute
)

type DeviceLinkGrantIssuerConfig struct {
	Database *gorm.DB
	Routes   PeerRouteResolver
	Signer   remoteauth.DeviceLinkGrantIssuer
	GrantTTL time.Duration
	Now      func() time.Time
}

// BrowserDeviceLinkGrantIssuer mints only v2 device-scoped grants. It shares
// neither the claim layout nor the subprotocol transport of v1 Peer tickets.
type BrowserDeviceLinkGrantIssuer struct {
	db       *gorm.DB
	routes   PeerRouteResolver
	signer   remoteauth.DeviceLinkGrantIssuer
	grantTTL time.Duration
	now      func() time.Time
}

func NewBrowserDeviceLinkGrantIssuer(config DeviceLinkGrantIssuerConfig) (*BrowserDeviceLinkGrantIssuer, error) {
	grantTTL := config.GrantTTL
	if config.Database == nil || config.Routes == nil || config.Signer.Issuer == "" || config.Signer.KeyID == "" ||
		len(config.Signer.PrivateKey) != ed25519.PrivateKeySize || grantTTL < 0 || (grantTTL > 0 && grantTTL < 5*time.Second) || grantTTL > MaximumDeviceLinkGrantTTL {
		return nil, errors.New("device link grant issuer configuration is invalid")
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &BrowserDeviceLinkGrantIssuer{db: config.Database, routes: config.Routes, signer: config.Signer, grantTTL: grantTTL, now: now}, nil
}

func (issuer *BrowserDeviceLinkGrantIssuer) IssueDeviceLink(ctx context.Context, input DeviceLinkIssueInput) (DeviceLink, error) {
	if issuer == nil || input.UserID == uuid.Nil || input.ControllerID == uuid.Nil || input.TargetDeviceID == uuid.Nil ||
		len(input.ControllerPublicKey) != ed25519.PublicKeySize || len(input.TargetPublicKey) != ed25519.PublicKeySize ||
		input.ControllerGrantVersion == 0 || input.ControllerKeyVersion == 0 || input.TargetGrantVersion == 0 || input.TargetKeyVersion == 0 ||
		remoteauth.PublicKeyThumbprint(input.ControllerPublicKey) != input.ControllerKeyThumbprint || remoteauth.PublicKeyThumbprint(input.TargetPublicKey) != input.TargetKeyThumbprint ||
		!validIdempotencyKey(input.IdempotencyKey) {
		return DeviceLink{}, ErrInvalidInput
	}
	allowedScopes, err := normalizeScopes(input.AllowedScopes, allowedControllerScopes)
	if err != nil {
		return DeviceLink{}, ErrInvalidInput
	}
	grantTTL := issuer.grantTTL
	if input.RequestedMaximumLifetimeSec != nil {
		requested := time.Duration(*input.RequestedMaximumLifetimeSec) * time.Second
		if requested < 5*time.Second || requested > MaximumDeviceLinkGrantTTL {
			return DeviceLink{}, ErrInvalidInput
		}
		if grantTTL == 0 || requested < grantTTL {
			grantTTL = requested
		}
	}
	now := issuer.now().UTC()
	route, err := resolveDeviceLinkRoute(ctx, issuer.routes, input.TargetDeviceID.String(), now)
	if err != nil {
		return DeviceLink{}, err
	}
	relayNodeID, nodeErr := uuid.Parse(route.NodeID)
	relayCellID, cellErr := uuid.Parse(route.CellID)
	if nodeErr != nil || cellErr != nil || route.DeviceID != input.TargetDeviceID.String() || route.UserID != input.UserID.String() ||
		route.GrantVersion != input.TargetGrantVersion || route.ConnectionEpoch == 0 {
		return DeviceLink{}, ErrUnavailable
	}
	if route.ProtocolVersion != 2 {
		return DeviceLink{}, ErrProtocolVersion
	}
	relayURL, err := loadRelayV2Endpoint(ctx, issuer.db, relayNodeID, relayCellID, now)
	if err != nil {
		return DeviceLink{}, err
	}
	maximumLifetimeSeconds, expiresAt := deviceLinkGrantWindow(now, grantTTL)
	claims := remoteauth.DeviceLinkGrantClaims{
		Audience:                 remoteauth.DeviceLinkGrantAudience,
		GrantID:                  uuid.NewString(),
		ClientID:                 input.ControllerID.String(),
		DeviceID:                 input.TargetDeviceID.String(),
		RelayNodeID:              relayNodeID.String(),
		RelayCellID:              relayCellID.String(),
		TargetConnectionEpoch:    route.ConnectionEpoch,
		ClientIdentityKey:        base64.RawURLEncoding.EncodeToString(input.ControllerPublicKey),
		ClientKeyThumbprint:      input.ControllerKeyThumbprint,
		ClientIdentityKeyVersion: input.ControllerKeyVersion,
		DeviceKeyThumbprint:      input.TargetKeyThumbprint,
		DeviceIdentityKeyVersion: input.TargetKeyVersion,
		ClientGrantVersion:       input.ControllerGrantVersion,
		DeviceGrantVersion:       input.TargetGrantVersion,
		AllowedScopes:            allowedScopes,
		MaximumLifetimeSeconds:   maximumLifetimeSeconds,
		IssuedAt:                 now.Unix(),
		NotBefore:                now.Add(-time.Second).Unix(),
		ExpiresAt:                expiresAt.Unix(),
	}
	grant, err := issuer.signer.Sign(claims)
	if err != nil {
		return DeviceLink{}, ErrUnavailable
	}
	grantID, err := uuid.Parse(claims.GrantID)
	if err != nil {
		return DeviceLink{}, ErrUnavailable
	}
	return DeviceLink{
		GrantID: grantID, Grant: grant, ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), MaximumLifetimeSeconds: claims.MaximumLifetimeSeconds,
		RelayURL: relayURL, RelayNodeID: relayNodeID, RelayCellID: relayCellID, TargetConnectionEpoch: route.ConnectionEpoch,
		DeviceIdentityAlgorithm: "Ed25519", DeviceIdentityPublicKey: base64.RawURLEncoding.EncodeToString(input.TargetPublicKey),
		DeviceKeyThumbprint: input.TargetKeyThumbprint, DeviceIdentityKeyVersion: input.TargetKeyVersion,
	}, nil
}

func deviceLinkGrantWindow(now time.Time, grantTTL time.Duration) (uint32, time.Time) {
	if grantTTL <= 0 {
		return 0, remoteauth.PersistentDeviceLinkGrantExpiry()
	}
	return uint32(grantTTL / time.Second), now.Add(grantTTL)
}

func resolveDeviceLinkRoute(ctx context.Context, routes PeerRouteResolver, deviceID string, now time.Time) (relayrouter.Route, error) {
	var (
		route relayrouter.Route
		err   error
	)
	if contextualRoutes, ok := routes.(ContextPeerRouteResolver); ok {
		route, err = contextualRoutes.ResolveContext(ctx, deviceID, now)
	} else {
		route, err = routes.Resolve(deviceID, now)
	}
	if err == nil {
		return route, nil
	}
	if errors.Is(err, relayrouter.ErrRouteNotFound) {
		return relayrouter.Route{}, ErrNotFound
	}
	return relayrouter.Route{}, ErrUnavailable
}

func loadRelayV2Endpoint(ctx context.Context, db *gorm.DB, nodeID, cellID uuid.UUID, now time.Time) (string, error) {
	if db == nil {
		return "", ErrUnavailable
	}
	var row struct {
		Addresses jsonBytes `gorm:"column:addresses"`
	}
	if err := db.WithContext(ctx).Raw(`
		SELECT instance.addresses
		FROM relay_node_instances instance
		JOIN relay_node_installations installation
		  ON installation.id = instance.installation_id AND installation.current_instance_id = instance.id
		WHERE instance.id = ? AND instance.cell_id = ? AND instance.status = 'ready'
		  AND instance.lease_expires_at > ? AND installation.status = 'active'`, nodeID, cellID, now).Take(&row).Error; err != nil {
		return "", ErrUnavailable
	}
	var addresses []string
	if json.Unmarshal(row.Addresses, &addresses) != nil || len(addresses) == 0 || len(addresses) > 16 {
		return "", ErrUnavailable
	}
	for _, address := range addresses {
		if endpoint, endpointErr := validateRelayV2Endpoint(address); endpointErr == nil {
			return endpoint, nil
		}
	}
	return "", ErrUnavailable
}

func validateRelayV2Endpoint(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path != "/v2/connect" {
		return "", ErrUnavailable
	}
	return parsed.String(), nil
}
