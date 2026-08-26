package relaydirectory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type grpcStoreStub struct {
	identity      relaymanagement.NodeCertificateIdentity
	registered    relaymanagement.RegisterInstanceInput
	heartbeat     relaymanagement.HeartbeatInput
	registerError error
}

func (stub *grpcStoreStub) RegisterInstance(_ context.Context, identity relaymanagement.NodeCertificateIdentity, input relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error) {
	stub.identity = identity
	stub.registered = input
	if stub.registerError != nil {
		return relaymanagement.NodeInstance{}, stub.registerError
	}
	return relaymanagement.NodeInstance{ID: input.InstanceID, Status: "starting", LeaseExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (stub *grpcStoreStub) Heartbeat(_ context.Context, identity relaymanagement.NodeCertificateIdentity, input relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error) {
	stub.identity = identity
	stub.heartbeat = input
	return relaymanagement.HeartbeatResult{LeaseExpiresAt: time.Now().Add(time.Minute), RoutingReady: true}, nil
}

func (*grpcStoreStub) UnregisterInstance(context.Context, relaymanagement.NodeCertificateIdentity, uuid.UUID) error {
	return nil
}

func TestGRPCDirectoryUsesVerifiedCertificateIdentity(t *testing.T) {
	store := &grpcStoreStub{}
	client, cleanup, installationID, cellID, thumbprint := newGRPCTestClient(t, store)
	defer cleanup()
	capabilities, _ := structpb.NewStruct(map[string]any{"wss": true})
	instanceID := uuid.New()
	response, err := client.RegisterInstance(context.Background(), &remotev1.RegisterInstanceRequest{
		InstanceId: instanceID.String(), Version: "1.0.0", ProtocolVersion: 1,
		Addresses: []string{"10.0.0.17:8443"}, Capabilities: capabilities, StartedAt: timestamppb.Now(),
	})
	if err != nil {
		t.Fatalf("RegisterInstance() error = %v", err)
	}
	if response.InstanceId != instanceID.String() || response.Status != remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_STARTING {
		t.Fatalf("RegisterInstance() response = %+v", response)
	}
	if store.identity.InstallationID != installationID || store.identity.CellID != cellID || store.identity.Thumbprint != thumbprint {
		t.Fatalf("certificate identity = %+v", store.identity)
	}
	if store.registered.InstanceID != instanceID || store.registered.Version != "1.0.0" || store.registered.Capabilities["wss"] != true {
		t.Fatalf("registered input = %+v", store.registered)
	}

	heartbeat, err := client.Heartbeat(context.Background(), &remotev1.HeartbeatRequest{
		InstanceId: instanceID.String(), ActiveConnections: 7, MemoryBytes: 4096, Capabilities: capabilities,
	})
	if err != nil || !heartbeat.RoutingReady || store.heartbeat.ActiveConnections != 7 {
		t.Fatalf("Heartbeat() = %+v, %v; stored=%+v", heartbeat, err, store.heartbeat)
	}
	if _, err := client.RenewCertificate(context.Background(), &remotev1.RenewCertificateRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("RenewCertificate() code = %s, want Unimplemented", status.Code(err))
	}
}

func TestGRPCDirectoryMapsRevokedIdentityWithoutRetryableDetails(t *testing.T) {
	store := &grpcStoreStub{registerError: relaymanagement.ErrInstallationRevoked}
	client, cleanup, _, _, _ := newGRPCTestClient(t, store)
	defer cleanup()
	_, err := client.RegisterInstance(context.Background(), &remotev1.RegisterInstanceRequest{
		InstanceId: uuid.NewString(), Version: "1.0.0", ProtocolVersion: 1,
		StartedAt: timestamppb.Now(), Capabilities: &structpb.Struct{Fields: map[string]*structpb.Value{}},
	})
	if status.Code(err) != codes.PermissionDenied || !errors.Is(mapRPCError(err), relaymanagement.ErrInstallationRevoked) {
		t.Fatalf("revoked RegisterInstance() error = %v", err)
	}
}

func newGRPCTestClient(t *testing.T, store Store) (remotev1.RelayDirectoryServiceClient, func(), uuid.UUID, uuid.UUID, string) {
	t.Helper()
	now := time.Now().UTC()
	authority, err := relayidentity.LoadOrCreateDevelopmentCA(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := relayidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	installationID, cellID := uuid.New(), uuid.New()
	thumbprint := relayidentity.Thumbprint(publicKey)
	nodeCertificate, err := authority.IssueNode(publicKey, installationID.String(), cellID.String(), thumbprint, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyPEM, err := relayidentity.MarshalPrivateKeyPEM(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCertificate, err := tls.X509KeyPair(nodeCertificate.CertificatePEM, privateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate, err := authority.IssueServer([]string{"bufnet"}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverIdentity, err := tls.X509KeyPair(serverCertificate.CertificatePEM, serverCertificate.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(authority.CAPEM())
	service, err := NewGRPCService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewGRPCServer(service, credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverIdentity},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
	}))
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13, ServerName: "bufnet", RootCAs: roots,
			Certificates: []tls.Certificate{clientCertificate},
		})),
	)
	if err != nil {
		server.Stop()
		_ = listener.Close()
		t.Fatal(err)
	}
	cleanup := func() {
		_ = connection.Close()
		server.Stop()
		_ = listener.Close()
	}
	return remotev1.NewRelayDirectoryServiceClient(connection), cleanup, installationID, cellID, thumbprint
}
