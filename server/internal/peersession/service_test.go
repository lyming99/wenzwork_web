package peersession

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

type credentialReaderStub struct {
	credentials map[uuid.UUID]Credential
	err         error
}

func (stub credentialReaderStub) LoadPeerCredentials(_ context.Context, sourceID, targetID uuid.UUID) (map[uuid.UUID]Credential, error) {
	if stub.err != nil {
		return nil, stub.err
	}
	result := make(map[uuid.UUID]Credential, 2)
	for _, id := range []uuid.UUID{sourceID, targetID} {
		if credential, ok := stub.credentials[id]; ok {
			result[id] = credential
		}
	}
	return result, nil
}

type admissionStub struct {
	keys map[string]ed25519.PublicKey
	err  error
}

type routeResolverStub struct {
	route relayrouter.Route
	err   error
}

func (stub routeResolverStub) Resolve(_ string, _ time.Time) (relayrouter.Route, error) {
	return stub.route, stub.err
}

type endpointReaderStub struct {
	url string
	err error
}

type projectReaderStub struct {
	projects map[uuid.UUID]bool
	err      error
}

func (stub projectReaderStub) ProjectAvailable(_ context.Context, _, _ uuid.UUID, projectID uuid.UUID) (bool, error) {
	if stub.err != nil {
		return false, stub.err
	}
	return stub.projects[projectID], nil
}

func (stub endpointReaderStub) LoadRelayEndpoint(_ context.Context, _, _ uuid.UUID, _ time.Time) (string, error) {
	return stub.url, stub.err
}

func (stub admissionStub) VerifyPeerDeviceState(_ context.Context, deviceID string, _ uint64, _ string) (ed25519.PublicKey, error) {
	if stub.err != nil {
		return nil, stub.err
	}
	key, ok := stub.keys[deviceID]
	if !ok {
		return nil, errors.New("missing projected credential")
	}
	return key, nil
}

type memoryIdempotencyStore struct {
	records map[string]struct {
		hash   string
		claims remoteauth.Claims
	}
}

func (store *memoryIdempotencyStore) Reserve(_ context.Context, userID, deviceID, key, requestHash string, proposed remoteauth.Claims, _ time.Duration) (remoteauth.Claims, error) {
	if store.records == nil {
		store.records = make(map[string]struct {
			hash   string
			claims remoteauth.Claims
		})
	}
	index := userID + ":" + deviceID + ":" + key
	if existing, ok := store.records[index]; ok {
		if existing.hash != requestHash {
			return remoteauth.Claims{}, ErrIdempotencyConflict
		}
		return existing.claims, nil
	}
	store.records[index] = struct {
		hash   string
		claims remoteauth.Claims
	}{requestHash, proposed}
	return proposed, nil
}

func peerServiceFixture(t *testing.T) (*Service, remoteauth.Verifier, IssueInput, *memoryIdempotencyStore, map[uuid.UUID]Credential) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sourcePublic, _, _ := ed25519.GenerateKey(rand.Reader)
	targetPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	userID, sourceID, targetID := uuid.New(), uuid.New(), uuid.New()
	nodeID, cellID := uuid.New(), uuid.New()
	credentials := map[uuid.UUID]Credential{
		sourceID: {
			DeviceID: sourceID, UserID: userID, IdentityPublicKey: sourcePublic,
			PublicKeyThumbprint: remoteauth.PublicKeyThumbprint(sourcePublic), KeyVersion: 2, GrantVersion: 3,
			Scopes:       []string{"remote.connect", "remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.file.send", "remote.peer.file.receive"},
			Capabilities: []string{"relay.ping", "remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.file.send", "remote.peer.file.receive"}, Status: "active",
		},
		targetID: {
			DeviceID: targetID, UserID: userID, IdentityPublicKey: targetPublic,
			PublicKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic), KeyVersion: 5, GrantVersion: 7,
			Scopes:       []string{"remote.connect", "remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.file.send", "remote.peer.file.receive"},
			Capabilities: []string{"relay.ping", "remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.file.send", "remote.peer.file.receive"}, Status: "active",
		},
	}
	store := &memoryIdempotencyStore{}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	service, err := NewService(Config{
		Credentials: credentialReaderStub{credentials: credentials},
		Admission: admissionStub{keys: map[string]ed25519.PublicKey{
			sourceID.String(): sourcePublic, targetID.String(): targetPublic,
		}},
		Routes: routeResolverStub{route: relayrouter.Route{
			DeviceID: targetID.String(), UserID: userID.String(), NodeID: nodeID.String(), CellID: cellID.String(),
			GrantVersion: 7, ConnectionEpoch: 42,
		}},
		Endpoints:   endpointReaderStub{url: "wss://relay-2.example.test/v1/connect"},
		Projects:    projectReaderStub{projects: map[uuid.UUID]bool{}},
		Issuer:      remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "peer-key-1", PrivateKey: privateKey},
		Idempotency: store, TicketTTL: time.Minute, MaxDuration: 15 * time.Minute, MaxBytes: 16 << 20,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, remoteauth.Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"peer-key-1": publicKey}}, IssueInput{
		UserID: userID, SessionID: uuid.New(), SourceDeviceID: sourceID, TargetDeviceID: targetID,
		Scope: "remote.peer.query", IdempotencyKey: "peer-ticket-0001",
	}, store, credentials
}

func TestIssueBindsProjectIntoTicketAndIdempotencyFence(t *testing.T) {
	service, verifier, input, _, _ := peerServiceFixture(t)
	projectID, otherProjectID := uuid.New(), uuid.New()
	service.projects = projectReaderStub{projects: map[uuid.UUID]bool{projectID: true, otherProjectID: true}}
	input.Scope = "remote.peer.file.receive"
	input.ProjectID = &projectID
	input.IdempotencyKey = "peer-project-0001"
	result, err := service.Issue(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(result.PeerSessionTicket, "relay-peer", time.Date(2026, 8, 7, 12, 0, 30, 0, time.UTC))
	if err != nil || claims.ProjectID != projectID.String() || !claims.HasScope(input.Scope) {
		t.Fatalf("project-bound claims = %+v, %v", claims, err)
	}
	changed := input
	changed.ProjectID = &otherProjectID
	if _, err := service.Issue(t.Context(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed project idempotency error = %v", err)
	}

	missing := input
	missing.ProjectID = nil
	missing.IdempotencyKey = "peer-project-0002"
	if _, err := service.Issue(t.Context(), missing); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing project error = %v", err)
	}
	unknownProjectID := uuid.New()
	unknown := input
	unknown.ProjectID = &unknownProjectID
	unknown.IdempotencyKey = "peer-project-0003"
	if _, err := service.Issue(t.Context(), unknown); !errors.Is(err, ErrTargetForbidden) {
		t.Fatalf("unknown project error = %v", err)
	}
	deviceQuery := input
	deviceQuery.Scope = "remote.peer.query"
	deviceQuery.IdempotencyKey = "peer-project-0004"
	if _, err := service.Issue(t.Context(), deviceQuery); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("device query with project error = %v", err)
	}
}

func TestIssueFileBindsAggregateLimitsAndResumeToOriginalTransfer(t *testing.T) {
	service, verifier, peerInput, _, credentials := peerServiceFixture(t)
	input := FileIssueInput{
		UserID: peerInput.UserID, SessionID: peerInput.SessionID, SourceDeviceID: peerInput.SourceDeviceID,
		TargetDeviceID: peerInput.TargetDeviceID, Direction: "push",
		RequestedLimits: FileRequestedLimits{TotalBytes: 4 << 20, FileCount: 12}, IdempotencyKey: "file-ticket-0001",
	}
	first, err := service.IssueFile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.TransferID == uuid.Nil || first.Limits.MaxTotalBytes != 4<<20 || first.Limits.MaxFileCount != 12 ||
		first.Limits.MaxManifestBytes != 1<<20 || first.Limits.AllowedChunkSize != 64<<10 || !first.RequireLocalApproval ||
		first.TargetKeyThumbprint != credentials[input.TargetDeviceID].PublicKeyThumbprint {
		t.Fatalf("IssueFile() = %+v", first)
	}
	claims, err := verifier.Verify(first.FileTicket, "relay-file", time.Date(2026, 8, 7, 12, 0, 30, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if claims.TransferID != first.TransferID.String() || claims.Subject != input.SourceDeviceID.String() || claims.Direction != "push" ||
		claims.SourceKeyVersion != 2 || claims.TargetKeyVersion != 5 || claims.SourceCredentialType != "device" ||
		claims.RelayNodeID == "" || claims.RelayCellID == "" || claims.TargetConnectionEpoch != 42 ||
		claims.ValidateFile(input.SourceDeviceID.String(), input.TargetDeviceID.String(),
			credentials[input.SourceDeviceID].PublicKeyThumbprint, credentials[input.TargetDeviceID].PublicKeyThumbprint, 3, 7) != nil {
		t.Fatalf("File claims = %+v", claims)
	}
	second, err := service.IssueFile(context.Background(), input)
	if err != nil || second != first {
		t.Fatalf("idempotent File Ticket = %+v, %v", second, err)
	}
	resume := input
	resume.IdempotencyKey = "file-ticket-0002"
	resume.PreviousFileTicket = first.FileTicket
	resumed, err := service.IssueFile(context.Background(), resume)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.TransferID != first.TransferID || resumed.FileTicket == first.FileTicket {
		t.Fatalf("resumed ticket = %+v, first = %+v", resumed, first)
	}
}

func TestIssueFileFailsClosedOnProjectAndPreviousTicketTampering(t *testing.T) {
	service, _, peerInput, _, credentials := peerServiceFixture(t)
	base := FileIssueInput{
		UserID: peerInput.UserID, SessionID: peerInput.SessionID, SourceDeviceID: peerInput.SourceDeviceID,
		TargetDeviceID: peerInput.TargetDeviceID, Direction: "push",
		RequestedLimits: FileRequestedLimits{TotalBytes: 1024, FileCount: 1}, IdempotencyKey: "file-ticket-1001",
	}
	for name, mutate := range map[string]func(*FileIssueInput){
		"unverified project": func(input *FileIssueInput) { value := uuid.New(); input.ProjectID = &value },
		"tampered previous":  func(input *FileIssueInput) { input.PreviousFileTicket = "not-a-signed-file-ticket" },
		"zero bytes":         func(input *FileIssueInput) { input.RequestedLimits.TotalBytes = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := service.IssueFile(context.Background(), input); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("IssueFile() error = %v", err)
			}
		})
	}
	source := credentials[base.SourceDeviceID]
	source.Scopes = []string{"remote.connect", "remote.peer.file.receive"}
	credentials[base.SourceDeviceID] = source
	if _, err := service.IssueFile(context.Background(), base); err != nil {
		t.Fatalf("file transfer must not depend on a relay scope grant: %v", err)
	}
}

func TestIssueBindsBothActiveDevicesAndAppliesRequestedLimits(t *testing.T) {
	service, verifier, input, _, credentials := peerServiceFixture(t)
	duration, bytes := uint32(120), uint64(4096)
	input.RequestedMaxDurationSeconds, input.RequestedMaxBytes = &duration, &bytes

	result, err := service.Issue(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID == uuid.Nil || result.MaxDurationSeconds != duration || result.MaxBytes != bytes ||
		result.TargetKeyThumbprint != credentials[input.TargetDeviceID].PublicKeyThumbprint ||
		result.RelayURL != "wss://relay-2.example.test/v1/connect" || result.RelayNodeID == uuid.Nil ||
		result.RelayCellID == uuid.Nil || result.TargetConnectionEpoch != 42 {
		t.Fatalf("Issue() result = %+v", result)
	}
	claims, err := verifier.Verify(result.PeerSessionTicket, "relay-peer", time.Date(2026, 8, 7, 12, 0, 30, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if claims.SessionID != result.SessionID.String() || claims.SourceDeviceID != input.SourceDeviceID.String() ||
		claims.TargetDeviceID != input.TargetDeviceID.String() || claims.SourceGrantVersion != 3 || claims.TargetGrantVersion != 7 ||
		claims.MaxBytes != bytes || !claims.HasScope("remote.peer.query") || claims.RelayNodeID != result.RelayNodeID.String() ||
		claims.RelayCellID != result.RelayCellID.String() || claims.TargetConnectionEpoch != 42 ||
		claims.Confirmation != credentials[input.SourceDeviceID].PublicKeyThumbprint {
		t.Fatalf("signed claims = %+v", claims)
	}
	if got := result.ExpiresAt.Sub(time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)); got != time.Minute {
		t.Fatalf("ticket TTL = %v", got)
	}
}

func TestIssueAllowsThirtyDayAdmissionTicket(t *testing.T) {
	base, verifier, input, _, _ := peerServiceFixture(t)
	service, err := NewService(Config{
		Credentials: base.credentials, Routes: base.routes, Endpoints: base.endpoints, Projects: base.projects,
		Issuer: base.issuer, Idempotency: base.idempotency, TicketTTL: DefaultTicketTTL,
		MaxDuration: DefaultMaxDuration, MaxBytes: base.maxBytes, Now: base.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	requested := uint32(DefaultMaxDuration / time.Second)
	input.RequestedMaxDurationSeconds = &requested
	result, err := service.Issue(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.MaxDurationSeconds != requested || result.ExpiresAt.Sub(time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)) != DefaultTicketTTL {
		t.Fatalf("long-lived ticket result = %+v", result)
	}
	if _, err := verifier.Verify(result.PeerSessionTicket, "relay-peer", result.ExpiresAt.Add(-time.Second)); err != nil {
		t.Fatalf("verify long-lived ticket: %v", err)
	}
}

func TestIssueAllowsDedicatedAIConfigurationScope(t *testing.T) {
	service, verifier, input, _, _ := peerServiceFixture(t)
	input.Scope = "remote.peer.ai.config"
	input.IdempotencyKey = "peer-ai-config-0001"
	result, err := service.Issue(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(result.PeerSessionTicket, "relay-peer", time.Date(2026, 8, 7, 12, 0, 30, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !claims.HasScope("remote.peer.ai.config") {
		t.Fatalf("signed claims scopes = %#v", claims.Scopes)
	}
}

func TestIssueRequiresAnOnlineTargetRoute(t *testing.T) {
	service, _, input, _, _ := peerServiceFixture(t)
	service.routes = routeResolverStub{err: relayrouter.ErrRouteNotFound}
	if _, err := service.Issue(context.Background(), input); !errors.Is(err, ErrTargetOffline) {
		t.Fatalf("Issue() error = %v, want ErrTargetOffline", err)
	}
}

func TestIssueRejectsAnUnsafeOrNonSpecificRelayEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"https://relay-2.example.test/v1/connect",
		"wss://relay-2.example.test/",
		"wss://relay-2.example.test/v1/connect?node=other",
		"wss://secret@relay-2.example.test/v1/connect",
	} {
		t.Run(endpoint, func(t *testing.T) {
			service, _, input, _, _ := peerServiceFixture(t)
			service.endpoints = endpointReaderStub{url: endpoint}
			if _, err := service.Issue(context.Background(), input); !errors.Is(err, ErrRelayUnavailable) {
				t.Fatalf("Issue() error = %v, want ErrRelayUnavailable", err)
			}
		})
	}
}

func TestRelayEndpointAllowsDirectAndSecureWebSocket(t *testing.T) {
	for _, endpoint := range []string{
		"ws://relay-2.example.test/v1/connect",
		"wss://relay-2.example.test/v1/connect",
	} {
		got, err := validateRelayEndpoint(endpoint)
		if err != nil || got != endpoint {
			t.Fatalf("validateRelayEndpoint(%q) = %q, %v", endpoint, got, err)
		}
	}
}

func TestIssueIsIdempotentWithoutPersistingTicketPlaintext(t *testing.T) {
	service, _, input, store, _ := peerServiceFixture(t)
	first, err := service.Issue(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Issue(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent results differ: first=%+v second=%+v", first, second)
	}
	for _, record := range store.records {
		if record.claims.SessionID == "" || record.claims.JWTID == "" {
			t.Fatalf("stored metadata = %+v", record)
		}
	}
	changedDuration := uint32(30)
	input.RequestedMaxDurationSeconds = &changedDuration
	if _, err := service.Issue(context.Background(), input); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed idempotent request error = %v", err)
	}
}

func TestIssueRejectsCrossAccountInactiveAndOverLimitRequests(t *testing.T) {
	for name, testCase := range map[string]struct {
		mutate func(*IssueInput, map[uuid.UUID]Credential)
		wanted error
	}{
		"cross account target": {func(input *IssueInput, credentials map[uuid.UUID]Credential) {
			target := credentials[input.TargetDeviceID]
			target.UserID = uuid.New()
			credentials[input.TargetDeviceID] = target
		}, ErrTargetForbidden},
		"inactive target": {func(input *IssueInput, credentials map[uuid.UUID]Credential) {
			target := credentials[input.TargetDeviceID]
			target.Status = "revoked"
			credentials[input.TargetDeviceID] = target
		}, ErrDeviceInactive},
		"over byte limit": {func(input *IssueInput, _ map[uuid.UUID]Credential) {
			value := uint64(DefaultMaxBytes + 1)
			input.RequestedMaxBytes = &value
		}, ErrInvalidRequest},
		"unverified project": {func(input *IssueInput, _ map[uuid.UUID]Credential) {
			value := uuid.New()
			input.ProjectID = &value
		}, ErrInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			service, _, input, _, credentials := peerServiceFixture(t)
			testCase.mutate(&input, credentials)
			if _, err := service.Issue(context.Background(), input); !errors.Is(err, testCase.wanted) {
				t.Fatalf("Issue() error = %v, want %v", err, testCase.wanted)
			}
		})
	}
}
