package relaydirectory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Client struct {
	connection *grpc.ClientConn
	rpc        remotev1.RelayDirectoryServiceClient
}

func NewClient(baseURL string, certificatePEM, privateKeyPEM, caPEM []byte) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("Relay Directory URL must be an absolute HTTPS origin")
	}
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load Relay node TLS identity: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Relay Directory CA certificate is invalid")
	}
	connection, err := grpc.NewClient(parsed.Host,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate},
		})),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20), grpc.MaxCallSendMsgSize(1<<20)),
	)
	if err != nil {
		return nil, fmt.Errorf("create Relay Directory gRPC client: %w", err)
	}
	return &Client{connection: connection, rpc: remotev1.NewRelayDirectoryServiceClient(connection)}, nil
}

func (client *Client) Close() error {
	if client == nil || client.connection == nil {
		return nil
	}
	return client.connection.Close()
}

func (client *Client) Register(ctx context.Context, input relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error) {
	if input.ProtocolVersion <= 0 {
		return relaymanagement.NodeInstance{}, relaymanagement.ErrInvalidInput
	}
	capabilities, err := structpb.NewStruct(input.Capabilities)
	if err != nil {
		return relaymanagement.NodeInstance{}, relaymanagement.ErrInvalidInput
	}
	response, err := client.rpc.RegisterInstance(ctx, &remotev1.RegisterInstanceRequest{
		InstanceId: input.InstanceID.String(), Version: input.Version, ProtocolVersion: uint32(input.ProtocolVersion),
		Addresses: input.Addresses, Capabilities: capabilities, StartedAt: timestamppb.New(input.StartedAt),
	})
	if err != nil {
		return relaymanagement.NodeInstance{}, mapRPCError(err)
	}
	instanceID, err := uuid.Parse(response.InstanceId)
	if err != nil || response.LeaseExpiresAt == nil || response.LeaseExpiresAt.CheckValid() != nil {
		return relaymanagement.NodeInstance{}, errors.New("Relay Directory returned an invalid registration response")
	}
	return relaymanagement.NodeInstance{
		ID: instanceID, Status: localInstanceStatus(response.Status), LeaseExpiresAt: response.LeaseExpiresAt.AsTime().UTC(),
	}, nil
}

func (client *Client) Heartbeat(ctx context.Context, input relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error) {
	if input.ActiveConnections < 0 || input.ActiveFileTransfers < 0 || input.MemoryBytes < 0 ||
		input.IngressMbps < 0 || math.IsNaN(input.IngressMbps) || math.IsInf(input.IngressMbps, 0) ||
		input.EgressMbps < 0 || math.IsNaN(input.EgressMbps) || math.IsInf(input.EgressMbps, 0) ||
		input.WriteLoopLagMS < 0 || math.IsNaN(input.WriteLoopLagMS) || math.IsInf(input.WriteLoopLagMS, 0) {
		return relaymanagement.HeartbeatResult{}, relaymanagement.ErrInvalidInput
	}
	capabilities, err := structpb.NewStruct(input.Capabilities)
	if err != nil {
		return relaymanagement.HeartbeatResult{}, relaymanagement.ErrInvalidInput
	}
	response, err := client.rpc.Heartbeat(ctx, &remotev1.HeartbeatRequest{
		InstanceId: input.InstanceID.String(), ActiveConnections: uint64(input.ActiveConnections),
		ActiveFileTransfers: uint64(input.ActiveFileTransfers), MemoryBytes: uint64(input.MemoryBytes),
		IngressMbps: input.IngressMbps, EgressMbps: input.EgressMbps, WriteLoopLagMs: input.WriteLoopLagMS,
		Addresses: input.Addresses, Capabilities: capabilities,
	})
	if err != nil {
		return relaymanagement.HeartbeatResult{}, mapRPCError(err)
	}
	if response.LeaseExpiresAt == nil || response.LeaseExpiresAt.CheckValid() != nil {
		return relaymanagement.HeartbeatResult{}, errors.New("Relay Directory returned an invalid heartbeat response")
	}
	return relaymanagement.HeartbeatResult{
		LeaseExpiresAt: response.LeaseExpiresAt.AsTime().UTC(), Drain: response.Drain,
		Revoked: response.Revoked, RoutingReady: response.RoutingReady,
	}, nil
}

func (client *Client) Unregister(ctx context.Context, instanceID uuid.UUID) error {
	_, err := client.rpc.UnregisterInstance(ctx, &remotev1.UnregisterInstanceRequest{InstanceId: instanceID.String()})
	return mapRPCError(err)
}

func mapRPCError(err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.InvalidArgument:
		return relaymanagement.ErrInvalidInput
	case codes.Unauthenticated:
		return relaymanagement.ErrIdentityMismatch
	case codes.PermissionDenied:
		return relaymanagement.ErrInstallationRevoked
	case codes.NotFound:
		return relaymanagement.ErrNotFound
	case codes.FailedPrecondition, codes.Aborted, codes.AlreadyExists:
		return relaymanagement.ErrConflict
	default:
		return fmt.Errorf("Relay Directory gRPC request failed (%s)", status.Code(err))
	}
}

func localInstanceStatus(value remotev1.RelayInstanceStatus) string {
	switch value {
	case remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_STARTING:
		return "starting"
	case remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_READY:
		return "ready"
	case remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_DRAINING:
		return "draining"
	case remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_STOPPED:
		return "stopped"
	case remotev1.RelayInstanceStatus_RELAY_INSTANCE_STATUS_OFFLINE:
		return "offline"
	default:
		return "unknown"
	}
}
