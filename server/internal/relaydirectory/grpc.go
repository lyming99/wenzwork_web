package relaydirectory

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Store interface {
	RegisterInstance(context.Context, relaymanagement.NodeCertificateIdentity, relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error)
	Heartbeat(context.Context, relaymanagement.NodeCertificateIdentity, relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error)
	UnregisterInstance(context.Context, relaymanagement.NodeCertificateIdentity, uuid.UUID) error
}

type GRPCService struct {
	remotev1.UnimplementedRelayDirectoryServiceServer
	store Store
	log   *slog.Logger
}

func NewGRPCService(store Store, logger *slog.Logger) (*GRPCService, error) {
	if store == nil {
		return nil, errors.New("Relay Directory store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GRPCService{store: store, log: logger}, nil
}

func (service *GRPCService) RegisterInstance(ctx context.Context, request *remotev1.RegisterInstanceRequest) (*remotev1.RegisterInstanceResponse, error) {
	identity, err := grpcIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if request == nil || request.ProtocolVersion > math.MaxInt32 || request.StartedAt == nil || request.StartedAt.CheckValid() != nil {
		return nil, status.Error(codes.InvalidArgument, "Relay registration request is invalid")
	}
	instanceID, err := uuid.Parse(request.InstanceId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Relay Instance ID is invalid")
	}
	capabilities := map[string]any{}
	if request.Capabilities != nil {
		capabilities = request.Capabilities.AsMap()
	}
	instance, err := service.store.RegisterInstance(ctx, identity, relaymanagement.RegisterInstanceInput{
		InstanceID: instanceID, Version: request.Version, ProtocolVersion: int(request.ProtocolVersion),
		Addresses: request.Addresses, Capabilities: capabilities, StartedAt: request.StartedAt.AsTime(),
	})
	if err != nil {
		return nil, service.mapError("register", err)
	}
	return &remotev1.RegisterInstanceResponse{
		InstanceId: instance.ID.String(), Status: protobufInstanceStatus(instance.Status),
		LeaseExpiresAt: timestamppb.New(instance.LeaseExpiresAt),
	}, nil
}

func (service *GRPCService) Heartbeat(ctx context.Context, request *remotev1.HeartbeatRequest) (*remotev1.HeartbeatResponse, error) {
	identity, err := grpcIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if request == nil || request.ActiveConnections > math.MaxInt64 || request.ActiveFileTransfers > math.MaxInt64 || request.MemoryBytes > math.MaxInt64 {
		return nil, status.Error(codes.InvalidArgument, "Relay heartbeat request is invalid")
	}
	instanceID, err := uuid.Parse(request.InstanceId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Relay Instance ID is invalid")
	}
	capabilities := map[string]any{}
	if request.Capabilities != nil {
		capabilities = request.Capabilities.AsMap()
	}
	result, err := service.store.Heartbeat(ctx, identity, relaymanagement.HeartbeatInput{
		InstanceID: instanceID, ActiveConnections: int64(request.ActiveConnections),
		ActiveFileTransfers: int64(request.ActiveFileTransfers), MemoryBytes: int64(request.MemoryBytes),
		IngressMbps: request.IngressMbps, EgressMbps: request.EgressMbps, WriteLoopLagMS: request.WriteLoopLagMs,
		Addresses: request.Addresses, Capabilities: capabilities,
	})
	if err != nil {
		return nil, service.mapError("heartbeat", err)
	}
	return &remotev1.HeartbeatResponse{
		LeaseExpiresAt: timestamppb.New(result.LeaseExpiresAt), Drain: result.Drain,
		Revoked: result.Revoked, RoutingReady: result.RoutingReady,
	}, nil
}

func (service *GRPCService) UnregisterInstance(ctx context.Context, request *remotev1.UnregisterInstanceRequest) (*remotev1.UnregisterInstanceResponse, error) {
	identity, err := grpcIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "Relay unregister request is invalid")
	}
	instanceID, err := uuid.Parse(request.InstanceId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Relay Instance ID is invalid")
	}
	if err := service.store.UnregisterInstance(ctx, identity, instanceID); err != nil {
		return nil, service.mapError("unregister", err)
	}
	return &remotev1.UnregisterInstanceResponse{}, nil
}

func (service *GRPCService) RenewCertificate(context.Context, *remotev1.RenewCertificateRequest) (*remotev1.RenewCertificateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "certificate renewal requires the production signer boundary to be configured")
}

func grpcIdentity(ctx context.Context) (relaymanagement.NodeCertificateIdentity, error) {
	connectionPeer, ok := peer.FromContext(ctx)
	if !ok || connectionPeer.AuthInfo == nil {
		return relaymanagement.NodeCertificateIdentity{}, status.Error(codes.Unauthenticated, "verified Relay mTLS identity is required")
	}
	tlsInfo, ok := connectionPeer.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
		return relaymanagement.NodeCertificateIdentity{}, status.Error(codes.Unauthenticated, "verified Relay mTLS identity is required")
	}
	identity, err := relaymanagement.DecodeCertificateIdentity(tlsInfo.State.PeerCertificates)
	if err != nil {
		return relaymanagement.NodeCertificateIdentity{}, status.Error(codes.Unauthenticated, "Relay certificate identity is invalid")
	}
	return identity, nil
}

func (service *GRPCService) mapError(operation string, err error) error {
	switch {
	case errors.Is(err, relaymanagement.ErrInvalidInput):
		return status.Error(codes.InvalidArgument, "Relay Directory request is invalid")
	case errors.Is(err, relaymanagement.ErrNotFound), errors.Is(err, relaymanagement.ErrIdentityMismatch):
		return status.Error(codes.Unauthenticated, "Relay identity is not registered")
	case errors.Is(err, relaymanagement.ErrInstallationRevoked):
		return status.Error(codes.PermissionDenied, "Relay identity is revoked")
	case errors.Is(err, relaymanagement.ErrConflict):
		return status.Error(codes.FailedPrecondition, "Relay installation or instance is not current")
	default:
		service.log.Error("Relay Directory gRPC request failed", "operation", operation, "error", err)
		return status.Error(codes.Unavailable, "Relay Directory is temporarily unavailable")
	}
}

func protobufInstanceStatus(value string) remotev1.RelayInstanceStatus {
	switch value {
	case "starting":
		return remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_STARTING
	case "ready":
		return remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_READY
	case "draining":
		return remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_DRAINING
	case "stopped":
		return remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_STOPPED
	case "offline":
		return remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_OFFLINE
	default:
		return remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_UNSPECIFIED
	}
}

func NewGRPCServer(service *GRPCService, transportCredentials credentials.TransportCredentials) (*grpc.Server, error) {
	if service == nil || transportCredentials == nil {
		return nil, errors.New("Relay Directory gRPC service and transport credentials are required")
	}
	server := grpc.NewServer(
		grpc.Creds(transportCredentials),
		grpc.MaxRecvMsgSize(1<<20),
		grpc.MaxSendMsgSize(1<<20),
		grpc.ConnectionTimeout(10*time.Second),
	)
	remotev1.RegisterRelayDirectoryServiceServer(server, service)
	return server, nil
}
