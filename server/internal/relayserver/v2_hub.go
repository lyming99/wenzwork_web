package relayserver

import (
	"errors"
	"sync"
	"time"

	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

const (
	// Carrier loss retains only the opaque Link route for five minutes. This is
	// aligned with the Device and Client recovery cache: it is long enough for a
	// network handoff, while preventing abandoned routes from becoming a second
	// long-lived authority store.
	v2LinkRouteGrace    = 5 * time.Minute
	v2MaximumLinkRoutes = 65_536
)

var ErrV2TransientRoute = errors.New("remote/v2 link route is temporarily stale")

// V2Hub holds Relay-visible routing state only. It never retains grant text,
// handshake secrets, project IDs, scopes, RPC bodies, paths, prompts or
// ciphertext copies after a frame is queued to the peer Carrier.
type V2Hub struct {
	mu             sync.Mutex
	carriers       map[string]*v2Carrier
	deviceCarriers map[string]string
	deviceRoutes   map[string]relayrouter.Route
	links          map[string]v2LinkRoute
	routeChanges   chan struct{}
}

type v2LinkRoute struct {
	linkID               string
	clientID             string
	deviceID             string
	clientCarrierID      string
	targetEpoch          uint64
	clientSuspendedUntil time.Time
	deviceSuspendedUntil time.Time
}

func NewV2Hub() *V2Hub {
	return &V2Hub{carriers: make(map[string]*v2Carrier), deviceCarriers: make(map[string]string), deviceRoutes: make(map[string]relayrouter.Route), links: make(map[string]v2LinkRoute), routeChanges: make(chan struct{}, 1)}
}

func (hub *V2Hub) attachClient(carrier *v2Carrier) error {
	if hub == nil || carrier == nil || carrier.role != v2CarrierClient || carrier.clientID == "" || carrier.deviceID == "" {
		return ErrV2Route
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if existing := hub.carriers[carrier.id]; existing != nil && existing != carrier {
		return ErrV2Route
	}
	hub.carriers[carrier.id] = carrier
	return nil
}

func (hub *V2Hub) attachDevice(carrier *v2Carrier, route relayrouter.Route) error {
	return hub.attachDeviceBeforePublish(carrier, route, nil)
}

// attachDeviceBeforePublish lets the handler enqueue CARRIER_READY while the
// new Device route is still private. Once the route is published, every Link
// frame queued by a Client is therefore ordered after READY on the Device's
// control queue.
func (hub *V2Hub) attachDeviceBeforePublish(carrier *v2Carrier, route relayrouter.Route, beforePublish func() error) error {
	if hub == nil || carrier == nil || carrier.role != v2CarrierDevice || carrier.deviceID == "" || route.DeviceID != carrier.deviceID || route.ConnectionID != carrier.id || route.ConnectionEpoch != carrier.epoch || route.ProtocolVersion != 2 {
		return ErrV2Route
	}
	hub.mu.Lock()
	now := time.Now()
	hub.pruneExpiredLocked(now)
	previousID := hub.deviceCarriers[carrier.deviceID]
	if existing := hub.carriers[carrier.id]; existing != nil && existing != carrier {
		hub.mu.Unlock()
		return ErrV2Route
	}
	previous := hub.carriers[previousID]
	// An authenticated Device replacement is a monotonic handoff. A delayed or
	// replayed lower/same epoch must never roll a stable resident route back or
	// evict the healthy Carrier that currently owns it.
	if previous != nil && previous != carrier && carrier.epoch <= previous.epoch {
		hub.mu.Unlock()
		return ErrV2Route
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			hub.mu.Unlock()
			return err
		}
	}
	hub.carriers[carrier.id] = carrier
	hub.deviceCarriers[carrier.deviceID] = carrier.id
	hub.deviceRoutes[carrier.deviceID] = route
	staleClientLinks := make(map[*v2Carrier][]string)
	for linkID, linkRoute := range hub.links {
		if linkRoute.deviceID != carrier.deviceID {
			continue
		}
		linkRoute.targetEpoch = carrier.epoch
		linkRoute.deviceSuspendedUntil = time.Time{}
		hub.links[linkID] = linkRoute
		if target := hub.carriers[linkRoute.clientCarrierID]; target != nil {
			staleClientLinks[target] = append(staleClientLinks[target], linkID)
		}
	}
	hub.mu.Unlock()
	if previous != nil && previous != carrier {
		previous.close()
	}
	hub.signalRouteChange()
	// A Device Carrier handoff invalidates only the cached physical route. Keep
	// every Client WebSocket alive and ask each affected logical Link to resume
	// against the newly authenticated Device Carrier.
	for client, linkIDs := range staleClientLinks {
		for _, linkID := range linkIDs {
			_ = sendV2LinkRouteStale(client, linkID)
		}
	}
	return nil
}

func (hub *V2Hub) detach(carrier *v2Carrier) {
	if hub == nil || carrier == nil {
		return
	}
	hub.mu.Lock()
	hub.pruneExpiredLocked(time.Now())
	changed := false
	if hub.carriers[carrier.id] == carrier {
		delete(hub.carriers, carrier.id)
		if carrier.role == v2CarrierDevice && hub.deviceCarriers[carrier.deviceID] == carrier.id {
			delete(hub.deviceCarriers, carrier.deviceID)
			delete(hub.deviceRoutes, carrier.deviceID)
			expires := time.Now().Add(v2LinkRouteGrace)
			for linkID, route := range hub.links {
				if route.deviceID == carrier.deviceID {
					route.deviceSuspendedUntil = expires
					hub.links[linkID] = route
				}
			}
			changed = true
		} else if carrier.role == v2CarrierClient {
			expires := time.Now().Add(v2LinkRouteGrace)
			for linkID, route := range hub.links {
				if route.clientCarrierID == carrier.id {
					route.clientSuspendedUntil = expires
					hub.links[linkID] = route
					changed = true
				}
			}
		}
	}
	hub.mu.Unlock()
	if changed {
		hub.signalRouteChange()
	}
}

// dropLink releases only opaque routing state. It deliberately does not close
// either Carrier: a malformed or cancelled Stream must never turn into a
// physical disconnect for unrelated Links/Channels.
func (hub *V2Hub) dropLink(linkID string) {
	if hub == nil || linkID == "" {
		return
	}
	hub.mu.Lock()
	delete(hub.links, linkID)
	hub.mu.Unlock()
}

// dropLinkFromCarrier applies a Link-fatal parse/routing failure only when the
// failing Carrier still owns that side of the route. A delayed frame from a
// superseded Device or a parallel Client must not delete the healthy binding.
func (hub *V2Hub) dropLinkFromCarrier(source *v2Carrier, linkID string) {
	if hub == nil || source == nil || linkID == "" {
		return
	}
	hub.mu.Lock()
	route, exists := hub.links[linkID]
	if exists {
		switch source.role {
		case v2CarrierClient:
			exists = route.clientCarrierID == source.id
		case v2CarrierDevice:
			exists = route.deviceID == source.deviceID && route.targetEpoch == source.epoch && hub.deviceCarriers[route.deviceID] == source.id
		default:
			exists = false
		}
	}
	if exists {
		delete(hub.links, linkID)
	}
	hub.mu.Unlock()
}

// ResidentRoutes is consumed by Relay Runtime heartbeats. It exposes only the
// same route fence fields already visible to the control plane.
func (hub *V2Hub) ResidentRoutes() []relayrouter.Route {
	if hub == nil {
		return nil
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.pruneExpiredLocked(time.Now())
	routes := make([]relayrouter.Route, 0, len(hub.deviceRoutes))
	for deviceID, route := range hub.deviceRoutes {
		carrier := hub.carriers[hub.deviceCarriers[deviceID]]
		if carrier != nil && carrier.id == route.ConnectionID && carrier.epoch == route.ConnectionEpoch {
			routes = append(routes, route)
		}
	}
	return routes
}

func (hub *V2Hub) RouteChanges() <-chan struct{} {
	if hub == nil {
		return nil
	}
	return hub.routeChanges
}

func (hub *V2Hub) RejectResident(deviceID, connectionID string, epoch uint64) bool {
	if hub == nil || deviceID == "" || connectionID == "" || epoch == 0 {
		return false
	}
	hub.mu.Lock()
	now := time.Now()
	hub.pruneExpiredLocked(now)
	carrier := hub.carriers[hub.deviceCarriers[deviceID]]
	if carrier == nil || carrier.id != connectionID || carrier.epoch != epoch {
		hub.mu.Unlock()
		return false
	}
	delete(hub.deviceCarriers, deviceID)
	delete(hub.deviceRoutes, deviceID)
	expires := now.Add(v2LinkRouteGrace)
	for linkID, route := range hub.links {
		if route.deviceID == deviceID {
			route.deviceSuspendedUntil = expires
			hub.links[linkID] = route
		}
	}
	hub.mu.Unlock()
	hub.signalRouteChange()
	carrier.close()
	return true
}

func (hub *V2Hub) CloseAll() {
	if hub == nil {
		return
	}
	hub.mu.Lock()
	carriers := make([]*v2Carrier, 0, len(hub.carriers))
	for _, carrier := range hub.carriers {
		carriers = append(carriers, carrier)
	}
	hub.carriers = make(map[string]*v2Carrier)
	hub.deviceCarriers = make(map[string]string)
	hub.deviceRoutes = make(map[string]relayrouter.Route)
	hub.links = make(map[string]v2LinkRoute)
	hub.mu.Unlock()
	hub.signalRouteChange()
	for _, carrier := range carriers {
		carrier.close()
	}
}

func (hub *V2Hub) ActiveCarriers() int64 {
	if hub == nil {
		return 0
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return int64(len(hub.carriers))
}

func (hub *V2Hub) ActiveLinkRoutes() int64 {
	if hub == nil {
		return 0
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.pruneExpiredLocked(time.Now())
	return int64(len(hub.links))
}

func (hub *V2Hub) signalRouteChange() {
	if hub == nil {
		return
	}
	select {
	case hub.routeChanges <- struct{}{}:
	default:
	}
}

func (hub *V2Hub) hasDevice(deviceID string, epoch uint64) bool {
	if hub == nil || deviceID == "" || epoch == 0 {
		return false
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	carrier := hub.carriers[hub.deviceCarriers[deviceID]]
	return carrier != nil && carrier.epoch == epoch
}

func (hub *V2Hub) routeClientLink(source *v2Carrier, claims remoteauth.DeviceLinkGrantClaims, link *remotev2.LinkEnvelope) error {
	if hub == nil || source == nil || source.role != v2CarrierClient || link == nil || claims.ClientID != source.clientID || claims.DeviceID != source.deviceID {
		return ErrV2Route
	}
	linkID, channelID, streamID := v2LinkMetadata(link)
	if linkID == "" || link.GetLinkId() != linkID {
		return ErrV2Route
	}
	hub.mu.Lock()
	hub.pruneExpiredLocked(time.Now())
	createdRoute := false
	superseded := make([]v2LinkRoute, 0, 1)
	if init := link.GetLinkInit(); init != nil {
		if init.GetLinkId() != linkID || init.GetGrantId() != claims.GrantID || init.GetDeviceConnectionGrant() == "" || init.GetClientId() != claims.ClientID || init.GetDeviceId() != claims.DeviceID ||
			init.GetRelayNodeId() != claims.RelayNodeID || init.GetRelayCellId() != claims.RelayCellID || init.GetTargetConnectionEpoch() != claims.TargetConnectionEpoch ||
			init.GetClientIdentityKeyVersion() != claims.ClientIdentityKeyVersion {
			hub.mu.Unlock()
			return ErrV2Route
		}
		if existing, exists := hub.links[linkID]; exists {
			if existing.clientID != claims.ClientID || existing.deviceID != claims.DeviceID {
				hub.mu.Unlock()
				return ErrV2Route
			}
			if current := hub.carriers[existing.clientCarrierID]; current != nil && current != source {
				// A parallel authenticated Carrier cannot steal a healthy Link by
				// replaying its LinkInit. Carrier replacement uses signed Resume.
				hub.mu.Unlock()
				return ErrV2Route
			}
			// A LinkInit retransmission is idempotent after the old Carrier left.
			existing.clientCarrierID = source.id
			existing.clientSuspendedUntil = time.Time{}
			hub.links[linkID] = existing
		} else {
			target := hub.carriers[hub.deviceCarriers[claims.DeviceID]]
			currentRoute := hub.deviceRoutes[claims.DeviceID]
			persistentRoute := claims.Persistent() && target != nil && target.epoch >= claims.TargetConnectionEpoch &&
				currentRoute.DeviceID == claims.DeviceID && currentRoute.ConnectionID == target.id && currentRoute.ConnectionEpoch == target.epoch &&
				currentRoute.GrantVersion == claims.DeviceGrantVersion
			if target == nil || (target.epoch != claims.TargetConnectionEpoch && !persistentRoute) {
				hub.mu.Unlock()
				_ = sendV2LinkRouteStale(source, linkID)
				return ErrV2TransientRoute
			}
			// One authenticated Controller has one Link per Device. A fresh proof-
			// bound Link replaces its abandoned route instead of growing Relay
			// memory on every reconnect or browser refresh.
			for existingID, existing := range hub.links {
				if existingID == linkID {
					continue
				}
				if (existing.clientID == claims.ClientID && existing.deviceID == claims.DeviceID) || existing.clientCarrierID == source.id {
					delete(hub.links, existingID)
					superseded = append(superseded, existing)
				}
			}
			if len(hub.links) >= v2MaximumLinkRoutes {
				hub.mu.Unlock()
				return ErrV2Route
			}
			hub.links[linkID] = v2LinkRoute{linkID: linkID, clientID: claims.ClientID, deviceID: claims.DeviceID, clientCarrierID: source.id, targetEpoch: target.epoch}
			createdRoute = true
		}
	} else {
		route, exists := hub.links[linkID]
		if !exists || route.clientID != claims.ClientID || route.deviceID != claims.DeviceID || route.clientCarrierID != source.id {
			hub.mu.Unlock()
			return rejectUnknownV2Link(source, linkID)
		}
		route.clientSuspendedUntil = time.Time{}
		hub.links[linkID] = route
	}
	resolvedRoute, routeExists := hub.links[linkID]
	target := hub.carriers[hub.deviceCarriers[claims.DeviceID]]
	targetReady := target != nil && target.epoch == resolvedRoute.targetEpoch
	hub.mu.Unlock()
	for _, stale := range superseded {
		if target != nil {
			_ = target.rejectStreamWithReason(stale.linkID, "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED)
		}
	}
	if !routeExists && !createdRoute {
		return rejectUnknownV2Link(source, linkID)
	}
	if !targetReady {
		return ErrV2TransientRoute
	}
	if err := target.enqueueLink(link); err != nil {
		if createdRoute {
			hub.dropLink(linkID)
		}
		if errors.Is(err, ErrV2QueueFull) {
			// The target's data-class queue is full, but Control has an
			// independent reservation. Notify both endpoints so the receiver does
			// not wait for a long RPC/Event timeout after the sender tears down.
			_ = source.rejectStream(linkID, channelID, streamID)
			_ = target.rejectStream(linkID, channelID, streamID)
			return nil
		}
		return ErrV2TransientRoute
	}
	return nil
}

// rejectUnknownV2Link reports a missing or no-longer-routable Link without
// channel/stream metadata. Metadata would make the desktop treat the response
// as a harmless per-Stream backpressure event, leaving an orphaned Link after
// the Device Carrier has restarted.
func rejectUnknownV2Link(source *v2Carrier, linkID string) error {
	if source == nil || linkID == "" {
		return ErrV2Route
	}
	return source.rejectStreamWithReason(linkID, "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_NOT_FOUND)
}

func (hub *V2Hub) routeDeviceLink(source *v2Carrier, link *remotev2.LinkEnvelope) error {
	if hub == nil || source == nil || source.role != v2CarrierDevice || link == nil {
		return ErrV2Route
	}
	linkID, channelID, streamID := v2LinkMetadata(link)
	if linkID == "" || link.GetLinkId() != linkID || link.GetLinkInit() != nil {
		return ErrV2Route
	}
	hub.mu.Lock()
	hub.pruneExpiredLocked(time.Now())
	route, exists := hub.links[linkID]
	if !exists || route.deviceID != source.deviceID || route.targetEpoch != source.epoch || hub.deviceCarriers[route.deviceID] != source.id {
		hub.mu.Unlock()
		return ErrV2Route
	}
	target := hub.carriers[route.clientCarrierID]
	hub.mu.Unlock()
	if target == nil {
		// Client Carrier loss is an expected resume condition. The Device Link
		// remains in its recovery TTL; do not punish the Device Carrier.
		return nil
	}
	if err := target.enqueueLink(link); err != nil {
		if errors.Is(err, ErrV2QueueFull) {
			_ = source.rejectStream(linkID, channelID, streamID)
			_ = target.rejectStream(linkID, channelID, streamID)
			return nil
		}
		return ErrV2TransientRoute
	}
	return nil
}

func (hub *V2Hub) resumeClient(source *v2Carrier, claims remoteauth.DeviceLinkGrantClaims, resume *remotev2.CarrierResume) error {
	if hub == nil || source == nil || source.role != v2CarrierClient || resume == nil || resume.GetLinkId() == "" || claims.ClientID != source.clientID || claims.DeviceID != source.deviceID {
		return ErrV2Route
	}
	hub.mu.Lock()
	hub.pruneExpiredLocked(time.Now())
	route, exists := hub.links[resume.GetLinkId()]
	if !exists || route.clientID != claims.ClientID || route.deviceID != claims.DeviceID {
		hub.mu.Unlock()
		return ErrV2Route
	}
	if current := hub.carriers[route.clientCarrierID]; current != nil && current != source {
		// Do not let a parallel Carrier silently steal a healthy Link binding.
		hub.mu.Unlock()
		return ErrV2Route
	}
	route.clientCarrierID = source.id
	route.clientSuspendedUntil = time.Time{}
	hub.links[route.linkID] = route
	target := hub.carriers[hub.deviceCarriers[route.deviceID]]
	hub.mu.Unlock()
	if target == nil || target.epoch != route.targetEpoch {
		if route.deviceSuspendedUntil.IsZero() || route.deviceSuspendedUntil.After(time.Now()) {
			return ErrV2TransientRoute
		}
		return ErrV2Route
	}
	if err := target.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Resume{Resume: resume}}, v2PriorityControl); err != nil {
		return ErrV2Route
	}
	return nil
}

func (hub *V2Hub) resumeDevice(source *v2Carrier, resume *remotev2.CarrierResume) error {
	if hub == nil || source == nil || source.role != v2CarrierDevice || resume == nil || resume.GetLinkId() == "" {
		return ErrV2Route
	}
	hub.mu.Lock()
	hub.pruneExpiredLocked(time.Now())
	route, exists := hub.links[resume.GetLinkId()]
	if !exists || route.deviceID != source.deviceID || route.targetEpoch != source.epoch || hub.deviceCarriers[route.deviceID] != source.id {
		hub.mu.Unlock()
		return ErrV2Route
	}
	target := hub.carriers[route.clientCarrierID]
	hub.mu.Unlock()
	if target == nil {
		return nil
	}
	if err := target.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Resume{Resume: resume}}, v2PriorityControl); err != nil {
		return ErrV2TransientRoute
	}
	return nil
}

// routeStreamRejected forwards only transport-level, per-Stream feedback to
// the peer Carrier.  It is needed for a resumed Carrier to learn that the
// Device discarded an expired Link, without treating the report as a Carrier
// failure or exposing any encrypted business content to the Relay.
func (hub *V2Hub) routeStreamRejected(source *v2Carrier, rejected *remotev2.CarrierStreamRejected) error {
	if hub == nil || source == nil || rejected == nil || rejected.GetLinkId() == "" || rejected.GetReason() == remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		return ErrV2Route
	}
	hub.mu.Lock()
	route, exists := hub.links[rejected.GetLinkId()]
	if !exists {
		hub.mu.Unlock()
		return ErrV2Route
	}
	var target *v2Carrier
	switch source.role {
	case v2CarrierClient:
		if route.clientID != source.clientID || route.deviceID != source.deviceID || route.clientCarrierID != source.id {
			hub.mu.Unlock()
			return ErrV2Route
		}
		target = hub.carriers[hub.deviceCarriers[route.deviceID]]
	case v2CarrierDevice:
		if route.deviceID != source.deviceID || hub.deviceCarriers[route.deviceID] != source.id {
			hub.mu.Unlock()
			return ErrV2Route
		}
		target = hub.carriers[route.clientCarrierID]
	default:
		hub.mu.Unlock()
		return ErrV2Route
	}
	hub.mu.Unlock()
	if target == nil {
		// The other Carrier may itself be reconnecting.  The Link's recovery
		// state remains at the endpoint and can emit this feedback again.
		return nil
	}
	return target.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_StreamRejected{StreamRejected: rejected}}, v2PriorityControl)
}

func (hub *V2Hub) pruneExpiredLocked(now time.Time) {
	if hub == nil {
		return
	}
	for linkID, route := range hub.links {
		clientExpired := !route.clientSuspendedUntil.IsZero() && !route.clientSuspendedUntil.After(now)
		deviceExpired := !route.deviceSuspendedUntil.IsZero() && !route.deviceSuspendedUntil.After(now)
		if !clientExpired && !deviceExpired {
			continue
		}
		delete(hub.links, linkID)
		if target := hub.carriers[hub.deviceCarriers[route.deviceID]]; target != nil {
			_ = target.rejectStreamWithReason(linkID, "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED)
		}
	}
}

func sendV2LinkRouteStale(carrier *v2Carrier, linkID string) error {
	if carrier == nil || carrier.queue == nil || linkID == "" {
		return ErrV2Route
	}
	return carrier.rejectStreamWithReason(linkID, "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_ROUTE_STALE)
}
