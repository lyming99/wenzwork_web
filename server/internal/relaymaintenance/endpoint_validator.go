package relaymaintenance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

var (
	ErrEndpointResolution = errors.New("Relay endpoint resolution failed")
	ErrEndpointUnsafe     = errors.New("Relay endpoint resolved to an unsafe address")
	ErrEndpointTLS        = errors.New("Relay endpoint TLS validation failed")
	ErrEndpointProtocol   = errors.New("Relay endpoint protocol validation failed")
	ErrEndpointIdentity   = errors.New("Relay endpoint identity validation failed")
)

const endpointResponseLimit = 16 << 10

type EndpointIdentityStore interface {
	ResolveEndpointIdentity(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (ed25519.PublicKey, error)
}

type EndpointValidator struct {
	Identities  EndpointIdentityStore
	Resolver    *net.Resolver
	LookupIP    func(context.Context, string) ([]net.IPAddr, error)
	DialContext func(context.Context, string, string) (net.Conn, error)
	RootCAs     *x509.CertPool
	Now         func() time.Time
	Random      io.Reader
}

func (validator EndpointValidator) Validate(ctx context.Context, endpoint relaymanagement.ManagedEndpoint) (relaymanagement.EndpointValidationResult, error) {
	if validator.Identities == nil || endpoint.ID == uuid.Nil || endpoint.CellID == uuid.Nil || endpoint.Status != "validating" {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointProtocol
	}
	parsed, err := url.Parse(endpoint.PublicEndpoint)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/v2/connect" {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointProtocol
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	addresses, err := validator.lookup(ctx, hostname)
	if err != nil || len(addresses) == 0 {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointResolution
	}
	approved := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if !isPublicAddress(address.IP) {
			return relaymanagement.EndpointValidationResult{}, ErrEndpointUnsafe
		}
		approved = append(approved, address.IP.String())
	}
	sort.Strings(approved)
	approved = compactStrings(approved)

	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(dialContext context.Context, network, _ string) (net.Conn, error) {
			dial := validator.DialContext
			if dial == nil {
				dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: -1}
				dial = dialer.DialContext
			}
			var joined error
			for _, address := range approved {
				connection, dialErr := dial(dialContext, network, net.JoinHostPort(address, port))
				if dialErr == nil {
					return connection, nil
				}
				joined = errors.Join(joined, dialErr)
			}
			return nil, joined
		},
		ForceAttemptHTTP2: false,
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12, ServerName: hostname, RootCAs: validator.RootCAs,
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	nonceBytes := make([]byte, 32)
	random := validator.Random
	if random == nil {
		random = rand.Reader
	}
	if _, err := io.ReadFull(random, nonceBytes); err != nil {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointProtocol
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	baseURL := "https://" + parsed.Host
	attestationRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/.well-known/wenzwork-relay?nonce="+url.QueryEscape(nonce), nil)
	if err != nil {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointProtocol
	}
	attestationRequest.Header.Set("Accept", "application/json")
	response, err := client.Do(attestationRequest)
	if err != nil {
		return relaymanagement.EndpointValidationResult{}, fmt.Errorf("%w: %v", ErrEndpointTLS, err)
	}
	certificateNotAfter, attestation, err := readAttestation(response)
	if err != nil {
		return relaymanagement.EndpointValidationResult{}, err
	}
	if attestation.Nonce != nonce || attestation.CellID != endpoint.CellID {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointIdentity
	}
	publicKey, err := validator.Identities.ResolveEndpointIdentity(ctx, attestation.CellID, attestation.InstallationID, attestation.InstanceID)
	if err != nil || relayidentity.VerifyEndpointAttestation(publicKey, attestation) != nil {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointIdentity
	}
	if !certificateNotAfter.After(validator.now().Add(time.Hour)) {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointTLS
	}

	connectRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v2/connect", nil)
	if err != nil {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointProtocol
	}
	connectRequest.Header.Set("Connection", "Upgrade")
	connectRequest.Header.Set("Upgrade", "websocket")
	connectRequest.Header.Set("Sec-WebSocket-Version", "13")
	connectRequest.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(nonceBytes[:16]))
	connectRequest.Header.Set("Sec-WebSocket-Protocol", "wenzwork-relay.v2")
	connectResponse, err := client.Do(connectRequest)
	if err != nil {
		return relaymanagement.EndpointValidationResult{}, fmt.Errorf("%w: %v", ErrEndpointProtocol, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(connectResponse.Body, 4<<10))
	_ = connectResponse.Body.Close()
	// v2 intentionally places the proof-bearing Grant in the first binary
	// CARRIER_HELLO, so a healthy endpoint performs the WebSocket upgrade and
	// then closes this unauthenticated validation socket. A redirect or a
	// different negotiated subprotocol is never a valid v2 Relay endpoint.
	if connectResponse.StatusCode != http.StatusSwitchingProtocols || connectResponse.Header.Get("Location") != "" || connectResponse.Header.Get("Sec-WebSocket-Protocol") != "wenzwork-relay.v2" {
		return relaymanagement.EndpointValidationResult{}, ErrEndpointProtocol
	}

	return relaymanagement.EndpointValidationResult{
		Checks: map[string]bool{
			"dns": true, "publicAddress": true, "tcp": true, "tls": true,
			"websocket": true, "cellIdentity": true,
		},
		ResolvedAddresses: approved, CertificateNotAfter: certificateNotAfter,
		InstallationID: attestation.InstallationID, InstanceID: attestation.InstanceID, CellID: attestation.CellID,
	}, nil
}

func (validator EndpointValidator) lookup(ctx context.Context, hostname string) ([]net.IPAddr, error) {
	if address := net.ParseIP(hostname); address != nil {
		return []net.IPAddr{{IP: address}}, nil
	}
	if validator.LookupIP != nil {
		return validator.LookupIP(ctx, hostname)
	}
	resolver := validator.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookupContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return resolver.LookupIPAddr(lookupContext, hostname)
}

func (validator EndpointValidator) now() time.Time {
	if validator.Now != nil {
		return validator.Now().UTC()
	}
	return time.Now().UTC()
}

func readAttestation(response *http.Response) (time.Time, relayidentity.EndpointAttestation, error) {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.TLS == nil || len(response.TLS.PeerCertificates) == 0 ||
		response.Header.Get("Location") != "" || !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		return time.Time{}, relayidentity.EndpointAttestation{}, ErrEndpointProtocol
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, endpointResponseLimit+1))
	if err != nil || len(payload) > endpointResponseLimit {
		return time.Time{}, relayidentity.EndpointAttestation{}, ErrEndpointProtocol
	}
	var attestation relayidentity.EndpointAttestation
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&attestation); err != nil {
		return time.Time{}, relayidentity.EndpointAttestation{}, ErrEndpointProtocol
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return time.Time{}, relayidentity.EndpointAttestation{}, ErrEndpointProtocol
	}
	return response.TLS.PeerCertificates[0].NotAfter.UTC(), attestation, nil
}

func isPublicAddress(address net.IP) bool {
	return address != nil && !address.IsUnspecified() && !address.IsLoopback() && !address.IsPrivate() &&
		!address.IsLinkLocalUnicast() && !address.IsLinkLocalMulticast() && !address.IsMulticast()
}

func compactStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
