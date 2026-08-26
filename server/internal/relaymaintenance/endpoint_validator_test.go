package relaymaintenance

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

type endpointIdentityStub struct {
	installationID uuid.UUID
	cellID         uuid.UUID
	instanceID     uuid.UUID
	publicKey      ed25519.PublicKey
}

func (stub endpointIdentityStub) ResolveEndpointIdentity(_ context.Context, cellID, installationID, instanceID uuid.UUID) (ed25519.PublicKey, error) {
	if cellID != stub.cellID || installationID != stub.installationID || instanceID != stub.instanceID {
		return nil, relaymanagement.ErrNotFound
	}
	return stub.publicKey, nil
}

func TestEndpointValidatorPinsPublicDNSAndVerifiesNodeAttestation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	identityPublic, identityPrivate, err := relayidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	installationID, cellID, instanceID := uuid.New(), uuid.New(), uuid.New()
	rootPool, certificate := endpointTestCertificate(t, now)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/.well-known/wenzwork-relay":
			attestation, signErr := relayidentity.SignEndpointAttestation(identityPrivate, relayidentity.EndpointAttestation{
				SchemaVersion: 1, Nonce: request.URL.Query().Get("nonce"), InstallationID: installationID,
				CellID: cellID, InstanceID: instanceID,
			})
			if signErr != nil {
				t.Error(signErr)
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(attestation)
		case "/v2/connect":
			connection, acceptErr := websocket.Accept(writer, request, &websocket.AcceptOptions{
				Subprotocols: []string{"wenzwork-relay.v2"}, CompressionMode: websocket.CompressionDisabled,
			})
			if acceptErr == nil {
				connection.CloseNow()
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	validator := EndpointValidator{
		Identities: endpointIdentityStub{installationID: installationID, cellID: cellID, instanceID: instanceID, publicKey: identityPublic},
		LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}, nil
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
		RootCAs: rootPool, Now: func() time.Time { return now }, Random: strings.NewReader(strings.Repeat("n", 32)),
	}
	result, err := validator.Validate(context.Background(), relaymanagement.ManagedEndpoint{
		ID: uuid.New(), CellID: cellID, Status: "validating",
		EndpointType: "domain", PublicEndpoint: "wss://relay.example.test:" + port + "/v2/connect",
	})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !result.Checks["tls"] || !result.Checks["websocket"] || !result.Checks["cellIdentity"] ||
		result.InstallationID != installationID || result.InstanceID != instanceID || len(result.ResolvedAddresses) != 1 {
		t.Fatalf("Validate() = %+v", result)
	}
}

func TestEndpointValidatorRejectsPrivateResolutionBeforeDial(t *testing.T) {
	dialed := false
	validator := EndpointValidator{
		Identities: endpointIdentityStub{},
		LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, context.Canceled
		},
	}
	_, err := validator.Validate(context.Background(), relaymanagement.ManagedEndpoint{
		ID: uuid.New(), CellID: uuid.New(), Status: "validating",
		EndpointType: "domain", PublicEndpoint: "wss://relay.example.test/v2/connect",
	})
	if !errors.Is(err, ErrEndpointUnsafe) || dialed {
		t.Fatalf("Validate() error = %v dialed=%t", err, dialed)
	}
}

func endpointTestCertificate(t *testing.T, now time.Time) (*x509.CertPool, tls.Certificate) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Relay Endpoint Test Root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "relay.example.test"},
		DNSNames: []string{"relay.example.test"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(12 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, rootTemplate, &serverKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	privateDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return pool, certificate
}
