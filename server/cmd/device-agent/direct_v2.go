package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

const (
	directV2Subprotocol       = "wenzwork-relay.v2"
	directV2HeartbeatInterval = 15 * time.Second
	directV2CarrierHeartbeat  = 25 * time.Second
	directV2HandshakeTimeout  = 10 * time.Second
	directV2MaximumGrantTTL   = 5 * time.Minute
	directV2AuthorizationTTL  = 45 * time.Second
	directV2LinkRequestLimit  = 8 << 10
	directV2LocalProofDomain  = "wenzwork-direct-controller:v1"
)

type directV2GrantRealm string

const (
	directV2HostGrant  directV2GrantRealm = "host"
	directV2LocalGrant directV2GrantRealm = "local"
)

type directV2Config struct {
	Enabled     bool
	IP          string
	Port        uint32
	TLSCertFile string
	TLSKeyFile  string
}

type directV2Registration struct {
	Enabled         bool
	IP              string
	Port            uint32
	ConnectionEpoch uint64
	TLSEnabled      bool
}

type directV2DeviceMetadata struct {
	Name         string
	Platform     string
	AgentVersion string
}

func parseDirectV2Config(enabledRaw, ipRaw, portRaw, tlsCertFileRaw, tlsKeyFileRaw string) (directV2Config, error) {
	enabledRaw = strings.TrimSpace(enabledRaw)
	if enabledRaw == "" {
		return directV2Config{}, nil
	}
	enabled, err := strconv.ParseBool(enabledRaw)
	if err != nil {
		return directV2Config{}, errors.New("WENZWORK_DEVICE_DIRECT_ENABLED must be true or false")
	}
	if !enabled {
		return directV2Config{}, nil
	}
	address, err := netip.ParseAddr(strings.TrimSpace(ipRaw))
	if err != nil {
		return directV2Config{}, errors.New("WENZWORK_DEVICE_DIRECT_IP must be an IP address")
	}
	address = address.Unmap()
	if address.IsUnspecified() || address.IsMulticast() {
		return directV2Config{}, errors.New("WENZWORK_DEVICE_DIRECT_IP must be a connectable unicast IP address")
	}
	port, err := strconv.ParseUint(strings.TrimSpace(portRaw), 10, 16)
	if err != nil || port == 0 {
		return directV2Config{}, errors.New("WENZWORK_DEVICE_DIRECT_PORT must be between 1 and 65535")
	}
	tlsCertFile, tlsKeyFile := strings.TrimSpace(tlsCertFileRaw), strings.TrimSpace(tlsKeyFileRaw)
	if (tlsCertFile == "") != (tlsKeyFile == "") {
		return directV2Config{}, errors.New("WENZWORK_DEVICE_DIRECT_TLS_CERT_FILE and WENZWORK_DEVICE_DIRECT_TLS_KEY_FILE must be configured together")
	}
	return directV2Config{Enabled: true, IP: address.String(), Port: uint32(port), TLSCertFile: tlsCertFile, TLSKeyFile: tlsKeyFile}, nil
}

func selectDirectV2AccessKey(enabled bool, configured, fallback string) (string, error) {
	if !enabled {
		return "", nil
	}
	configured, fallback = strings.TrimSpace(configured), strings.TrimSpace(fallback)
	if configured == "" {
		configured = fallback
	}
	if configured != "" && !validDeviceKey(configured) {
		return "", errors.New("device direct Access Key is invalid")
	}
	return configured, nil
}

type directV2Runtime struct {
	config          directV2Config
	connectionEpoch uint64
	nodeID          uuid.UUID
	cellID          uuid.UUID
	originPatterns  []string
	listener        net.Listener
	closeOnce       sync.Once
}

func prepareDirectV2Runtime(config directV2Config, controlURL *url.URL, state *agentState) (*directV2Runtime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if state == nil || controlURL == nil {
		return nil, errors.New("direct connection configuration is incomplete")
	}
	epoch, err := state.advanceConnectionEpoch()
	if err != nil {
		return nil, err
	}
	var certificate *tls.Certificate
	if config.TLSCertFile != "" {
		pair, pairErr := tls.LoadX509KeyPair(config.TLSCertFile, config.TLSKeyFile)
		if pairErr != nil || len(pair.Certificate) == 0 {
			return nil, errors.New("load device direct TLS certificate and key")
		}
		leaf, leafErr := x509.ParseCertificate(pair.Certificate[0])
		now := time.Now().UTC()
		if leafErr != nil || leaf.VerifyHostname(config.IP) != nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
			return nil, errors.New("device direct TLS certificate is not valid for the configured IP")
		}
		certificate = &pair
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(config.IP, strconv.FormatUint(uint64(config.Port), 10)))
	if err != nil {
		return nil, errors.New("bind device direct IP and port")
	}
	if certificate != nil {
		listener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{*certificate}, MinVersion: tls.VersionTLS12})
	}
	origins := directV2OriginPatterns(controlURL)
	if len(origins) == 0 {
		_ = listener.Close()
		return nil, errors.New("device direct browser origin is invalid")
	}
	return &directV2Runtime{
		config: config, connectionEpoch: epoch,
		nodeID:         uuid.NewSHA1(uuid.NameSpaceOID, []byte("wenzwork-direct-node:"+state.DeviceID.String())),
		cellID:         uuid.NewSHA1(uuid.NameSpaceOID, []byte("wenzwork-direct-cell:"+state.DeviceID.String())),
		originPatterns: origins, listener: listener,
	}, nil
}

func directV2OriginPatterns(controlURL *url.URL) []string {
	if controlURL == nil || (controlURL.Scheme != "http" && controlURL.Scheme != "https") || controlURL.Host == "" {
		return nil
	}
	return []string{controlURL.Scheme + "://" + controlURL.Host}
}

func (runtime *directV2Runtime) registration() directV2Registration {
	if runtime == nil {
		return directV2Registration{}
	}
	return directV2Registration{
		Enabled: true, IP: runtime.config.IP, Port: runtime.config.Port, ConnectionEpoch: runtime.connectionEpoch,
		TLSEnabled: runtime.config.TLSCertFile != "",
	}
}

func (runtime *directV2Runtime) connectionURL() string {
	if runtime == nil || !runtime.config.Enabled || runtime.config.IP == "" || runtime.config.Port == 0 {
		return ""
	}
	scheme := "ws"
	if runtime.config.TLSCertFile != "" {
		scheme = "wss"
	}
	return (&url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(runtime.config.IP, strconv.FormatUint(uint64(runtime.config.Port), 10)),
		Path:   "/v2/connect",
	}).String()
}

func (runtime *directV2Runtime) close() {
	if runtime == nil {
		return
	}
	runtime.closeOnce.Do(func() {
		if runtime.listener != nil {
			_ = runtime.listener.Close()
		}
	})
}

func (runtime *directV2Runtime) run(
	ctx context.Context,
	state *agentState,
	verifier remoteauth.DeviceLinkGrantVerifier,
	tokens *deviceTokenManager,
	hostRoutes *hostV2RouteCoordinator,
	initialHostDirect bool,
	directAccessKey string,
	metadata directV2DeviceMetadata,
) error {
	if runtime == nil || ctx == nil || state == nil || tokens == nil || runtime.listener == nil || verifier.Issuer == "" || len(verifier.Keys) == 0 {
		return errV2AgentCarrier
	}
	handler := newDirectV2Handler(state, runtime, verifier)
	handler.setEnabled(initialHostDirect)
	if err := handler.configureLocalAuthorization(directAccessKey, metadata); err != nil {
		return err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	serveResult := make(chan error, 1)
	go func() {
		err := server.Serve(runtime.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveResult <- err
	}()
	heartbeatResult := make(chan error, 1)
	go func() { heartbeatResult <- runtime.runHeartbeats(ctx, tokens, handler, hostRoutes) }()
	defer func() {
		handler.close()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
		runtime.close()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serveResult:
		if err == nil {
			return errV2AgentCarrier
		}
		return err
	case err := <-heartbeatResult:
		if err == nil {
			return errV2AgentCarrier
		}
		return err
	}
}

func (runtime *directV2Runtime) runHeartbeats(ctx context.Context, tokens *deviceTokenManager, handler *directV2Handler, hostRoutes *hostV2RouteCoordinator) error {
	backoff := 500 * time.Millisecond
	lastSuccess := time.Now()
	for {
		var state struct {
			Enabled bool `json:"enabled"`
		}
		err := tokens.doJSON(ctx, http.MethodPost, "/v1/device/direct-heartbeats", "", map[string]any{
			"ip": runtime.config.IP, "port": runtime.config.Port, "connectionEpoch": runtime.connectionEpoch,
			"tlsEnabled": runtime.config.TLSCertFile != "",
		}, &state)
		if err != nil && !errors.Is(err, errControlRetryable) {
			handler.setEnabled(false)
			if hostRoutes != nil {
				_ = hostRoutes.setDirect(ctx, false)
			}
			return err
		}
		wait := directV2HeartbeatInterval
		if err != nil {
			if time.Since(lastSuccess) >= directV2AuthorizationTTL {
				handler.setEnabled(false)
				if hostRoutes != nil {
					if transitionErr := hostRoutes.setDirect(ctx, false); transitionErr != nil {
						return transitionErr
					}
				}
			}
			wait = backoff
			backoff = min(backoff*2, 30*time.Second)
		} else {
			lastSuccess = time.Now()
			if state.Enabled {
				if hostRoutes != nil {
					if transitionErr := hostRoutes.setDirect(ctx, true); transitionErr != nil {
						return transitionErr
					}
				}
				handler.setEnabled(true)
			} else {
				handler.setEnabled(false)
				if hostRoutes != nil {
					if transitionErr := hostRoutes.setDirect(ctx, false); transitionErr != nil {
						return transitionErr
					}
				}
			}
			backoff = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

type directV2LocalAuthorization struct {
	enabled         bool
	accessKeyDigest [sha256.Size]byte
	issuer          remoteauth.DeviceLinkGrantIssuer
	verifier        remoteauth.DeviceLinkGrantVerifier
	metadata        directV2DeviceMetadata
	connectionURL   string
	allowedScopes   []string
}

type directV2DeviceLinkRequest struct {
	ControllerID             string `json:"controllerId"`
	ClientIdentityAlgorithm  string `json:"clientIdentityAlgorithm"`
	ClientIdentityPublicKey  string `json:"clientIdentityPublicKey"`
	ClientIdentityKeyVersion uint64 `json:"clientIdentityKeyVersion"`
	Proof                    string `json:"proof"`
}

type directV2DeviceView struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Platform           string `json:"platform"`
	AgentVersion       string `json:"agentVersion"`
	IdentityAlgorithm  string `json:"identityAlgorithm"`
	IdentityPublicKey  string `json:"identityPublicKey"`
	IdentityKeyVersion uint64 `json:"identityKeyVersion"`
	KeyThumbprint      string `json:"keyThumbprint"`
}

type directV2DeviceLinkView struct {
	GrantID                  string    `json:"grantId"`
	Grant                    string    `json:"deviceConnectionGrant"`
	ExpiresAt                time.Time `json:"expiresAt"`
	MaximumLifetimeSeconds   uint32    `json:"maximumLifetimeSeconds"`
	ConnectionMode           string    `json:"connectionMode"`
	ConnectionURL            string    `json:"connectionUrl"`
	RelayURL                 string    `json:"relayUrl"`
	RelayNodeID              string    `json:"relayNodeId"`
	RelayCellID              string    `json:"relayCellId"`
	TargetConnectionEpoch    uint64    `json:"targetConnectionEpoch"`
	DeviceIdentityAlgorithm  string    `json:"deviceIdentityAlgorithm"`
	DeviceIdentityPublicKey  string    `json:"deviceIdentityPublicKey"`
	DeviceKeyThumbprint      string    `json:"deviceKeyThumbprint"`
	DeviceIdentityKeyVersion uint64    `json:"deviceIdentityKeyVersion"`
}

type directV2DeviceLinkResponse struct {
	Device     directV2DeviceView     `json:"device"`
	DeviceLink directV2DeviceLinkView `json:"deviceLink"`
}

type directV2Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
}

type directV2Authentication struct {
	remoteauth.DeviceLinkGrantClaims
	realm directV2GrantRealm
}

type directV2RegistryEntry struct {
	registry *v2AgentLinkRegistry
	carrier  *v2AgentCarrier
	lastUsed time.Time
	realm    directV2GrantRealm
}

type directV2Handler struct {
	state          *agentState
	epoch          uint64
	nodeID         uuid.UUID
	cellID         uuid.UUID
	connectionURL  string
	originPatterns []string
	hostVerifier   remoteauth.DeviceLinkGrantVerifier
	local          directV2LocalAuthorization
	enabled        atomic.Bool
	slots          chan struct{}
	mu             sync.Mutex
	registries     map[string]*directV2RegistryEntry
}

func newDirectV2Handler(state *agentState, runtime *directV2Runtime, verifier remoteauth.DeviceLinkGrantVerifier) *directV2Handler {
	return &directV2Handler{
		state: state, epoch: runtime.connectionEpoch, nodeID: runtime.nodeID, cellID: runtime.cellID,
		connectionURL: runtime.connectionURL(), originPatterns: append([]string(nil), runtime.originPatterns...), hostVerifier: verifier,
		slots: make(chan struct{}, 2*v2MaximumLinksPerDevice), registries: make(map[string]*directV2RegistryEntry),
	}
}

func (handler *directV2Handler) configureLocalAuthorization(accessKey string, metadata directV2DeviceMetadata) error {
	if handler == nil || handler.state == nil {
		return errV2AgentCarrier
	}
	if accessKey == "" {
		return nil
	}
	metadata.Name, metadata.Platform, metadata.AgentVersion = strings.TrimSpace(metadata.Name), strings.TrimSpace(metadata.Platform), strings.TrimSpace(metadata.AgentVersion)
	if !validDeviceKey(accessKey) || metadata.Name == "" || metadata.AgentVersion == "" ||
		(metadata.Platform != "windows" && metadata.Platform != "macos" && metadata.Platform != "linux") {
		return errors.New("device direct local authorization configuration is invalid")
	}
	publicKey, ok := handler.state.identity.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize || handler.state.KeyVersion == 0 {
		return errV2AgentCarrier
	}
	privateKey := append(ed25519.PrivateKey(nil), handler.state.identity...)
	publicKey = append(ed25519.PublicKey(nil), publicKey...)
	issuer := "wenzwork-direct-agent:" + handler.state.DeviceID.String()
	keyID := "agent-identity-" + strconv.FormatUint(handler.state.KeyVersion, 10)
	allowedScopes := directV2LocalAllowedScopes(handler.state)
	if len(allowedScopes) == 0 {
		return errV2AgentCarrier
	}
	handler.local = directV2LocalAuthorization{
		enabled:         true,
		accessKeyDigest: sha256.Sum256([]byte(accessKey)),
		issuer:          remoteauth.DeviceLinkGrantIssuer{Issuer: issuer, KeyID: keyID, PrivateKey: privateKey},
		verifier:        remoteauth.DeviceLinkGrantVerifier{Issuer: issuer, Keys: map[string]ed25519.PublicKey{keyID: publicKey}},
		metadata:        metadata,
		connectionURL:   handler.connectionURL,
		allowedScopes:   allowedScopes,
	}
	return nil
}

func directV2LocalAllowedScopes(state *agentState) []string {
	capabilities := agentRegistrationCapabilities(state)
	result := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if strings.HasPrefix(capability, "remote.peer.") {
			result = append(result, capability)
		}
	}
	return result
}

func (handler *directV2Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil {
		http.Error(writer, "remote/v2 direct listener is unavailable", http.StatusServiceUnavailable)
		return
	}
	if request == nil || request.URL == nil {
		http.Error(writer, "remote/v2 direct handshake is invalid", http.StatusBadRequest)
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v2/direct/device-links" {
		handler.serveLocalDeviceLink(writer, request)
		return
	}
	if request.Method != http.MethodGet || request.URL.Path != "/v2/connect" {
		http.NotFound(writer, request)
		return
	}
	handler.serveCarrier(writer, request)
}

func (handler *directV2Handler) serveCarrier(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" || request.Header.Get("Authorization") != "" || !directV2HasOnlySubprotocol(request.Header.Values("Sec-WebSocket-Protocol")) {
		http.Error(writer, "remote/v2 direct handshake is invalid", http.StatusBadRequest)
		return
	}
	if !handler.enabled.Load() && !handler.local.enabled {
		http.Error(writer, "remote/v2 direct mode is not enabled", http.StatusServiceUnavailable)
		return
	}
	select {
	case handler.slots <- struct{}{}:
		defer func() { <-handler.slots }()
	default:
		http.Error(writer, "remote/v2 direct listener is busy", http.StatusServiceUnavailable)
		return
	}
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols: []string{directV2Subprotocol}, OriginPatterns: handler.originPatterns, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	if connection.Subprotocol() != directV2Subprotocol {
		_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 direct subprotocol required")
		return
	}
	connection.SetReadLimit(v2AgentMaximumCarrierFrame)
	handshakeContext, cancelHandshake := context.WithTimeout(request.Context(), directV2HandshakeTimeout)
	first, err := readV2AgentEnvelope(handshakeContext, connection)
	cancelHandshake()
	if err != nil || first.GetHello() == nil || first.GetPacketSequence() != 1 || first.GetAcknowledgedSequence() != 0 ||
		first.GetCarrierEpoch() == 0 || uuid.Validate(first.GetCarrierId()) != nil {
		_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 direct hello invalid")
		return
	}
	authentication, err := handler.authenticate(first, first.GetHello(), time.Now().UTC())
	if err != nil {
		// Keep the established Carrier close contract so the shared browser
		// connection layer discards this rejected Grant before retrying.
		_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 client authentication failed")
		return
	}
	if authentication.realm == directV2HostGrant && !handler.enabled.Load() {
		_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 Host direct mode is not enabled")
		return
	}
	claims := authentication.DeviceLinkGrantClaims
	carrier, err := newV2AgentCarrier(connection, first.GetCarrierId(), first.GetCarrierEpoch())
	if err != nil {
		return
	}
	carrier.targetConnectionEpoch = handler.epoch
	if carrier.acceptIncoming(first) != nil {
		carrier.close()
		return
	}
	registry := handler.bindRegistry(authentication.realm, claims.ClientID, carrier)
	if registry == nil {
		carrier.close()
		return
	}
	defer func() {
		handler.releaseRegistry(authentication.realm, claims.ClientID, carrier)
		carrier.close()
	}()
	if err := carrier.send(request.Context(), &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ready{Ready: &remotev2.CarrierReady{
		CarrierId: carrier.id, CarrierEpoch: carrier.epoch, HeartbeatIntervalSeconds: uint32(directV2CarrierHeartbeat / time.Second),
		ControlQueueByteLimit: v2AgentControlQueueBytes, InteractiveQueueByteLimit: v2AgentInteractiveBytes, BulkQueueByteLimit: v2AgentBulkQueueBytes,
	}}}, v2AgentControl); err != nil {
		return
	}
	verifier := handler.hostVerifier
	if authentication.realm == directV2LocalGrant {
		verifier = handler.local.verifier
	}
	_ = serveTargetV2WithOptions(request.Context(), carrier, directV2CarrierHeartbeat, registry, verifier, true)
}

func (handler *directV2Handler) serveLocalDeviceLink(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || !handler.local.enabled {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Vary", "Authorization")
	if request.URL.RawQuery != "" {
		writeDirectV2Problem(writer, http.StatusBadRequest, "direct_request_invalid", "直连授权请求无效", "请求地址不能包含查询参数。")
		return
	}
	if !handler.localAccessKeyAuthorized(request.Header.Values("Authorization")) {
		writeDirectV2Problem(writer, http.StatusUnauthorized, "direct_access_unauthorized", "直连访问密钥无效", "请检查设备 IP、端口和 Access Key。")
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || !directV2JSONParametersValid(parameters) {
		writeDirectV2Problem(writer, http.StatusUnsupportedMediaType, "direct_content_type_invalid", "直连授权请求格式无效", "Content-Type 必须为 application/json。")
		return
	}
	if request.ContentLength > directV2LinkRequestLimit {
		writeDirectV2Problem(writer, http.StatusRequestEntityTooLarge, "direct_request_too_large", "直连授权请求过大", "请缩小请求体后重试。")
		return
	}
	var input directV2DeviceLinkRequest
	limited := http.MaxBytesReader(writer, request.Body, directV2LinkRequestLimit)
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeDirectV2Problem(writer, http.StatusRequestEntityTooLarge, "direct_request_too_large", "直连授权请求过大", "请缩小请求体后重试。")
			return
		}
		writeDirectV2Problem(writer, http.StatusBadRequest, "direct_request_invalid", "直连授权请求无效", "请检查控制端身份字段。")
		return
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeDirectV2Problem(writer, http.StatusRequestEntityTooLarge, "direct_request_too_large", "直连授权请求过大", "请缩小请求体后重试。")
			return
		}
		writeDirectV2Problem(writer, http.StatusBadRequest, "direct_request_invalid", "直连授权请求无效", "请求体只能包含一个 JSON 对象。")
		return
	}
	response, err := handler.issueLocalDeviceLink(input, time.Now().UTC())
	if err != nil {
		// A malformed controller proof is intentionally indistinguishable from
		// an invalid Access Key to avoid turning the listener into an identity
		// or credential oracle.
		writeDirectV2Problem(writer, http.StatusUnauthorized, "direct_access_unauthorized", "直连访问密钥无效", "请检查设备 IP、端口和 Access Key。")
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(writer).Encode(response)
}

func directV2JSONParametersValid(parameters map[string]string) bool {
	if len(parameters) == 0 {
		return true
	}
	return len(parameters) == 1 && strings.EqualFold(parameters["charset"], "utf-8")
}

func (handler *directV2Handler) localAccessKeyAuthorized(values []string) bool {
	value := ""
	structured := false
	if len(values) == 1 {
		scheme, credential, found := strings.Cut(values[0], " ")
		value = credential
		structured = found && strings.EqualFold(scheme, "Bearer") && credential != "" && credential == strings.TrimSpace(credential) &&
			!strings.ContainsAny(credential, " \t\r\n,")
	}
	digest := sha256.Sum256([]byte(value))
	matched := subtle.ConstantTimeCompare(digest[:], handler.local.accessKeyDigest[:])
	structuredByte := byte(0)
	if structured {
		structuredByte = 1
	}
	return subtle.ConstantTimeByteEq(byte(matched), 1)&subtle.ConstantTimeByteEq(structuredByte, 1) == 1
}

func (handler *directV2Handler) issueLocalDeviceLink(input directV2DeviceLinkRequest, now time.Time) (directV2DeviceLinkResponse, error) {
	if handler == nil || !handler.local.enabled || handler.state == nil || handler.epoch == 0 || handler.nodeID == uuid.Nil || handler.cellID == uuid.Nil ||
		handler.local.connectionURL == "" || input.ClientIdentityAlgorithm != "Ed25519" || input.ClientIdentityKeyVersion == 0 {
		return directV2DeviceLinkResponse{}, errV2AgentCarrier
	}
	controllerID, err := uuid.Parse(input.ControllerID)
	if err != nil || controllerID.String() != input.ControllerID || controllerID == handler.state.DeviceID {
		return directV2DeviceLinkResponse{}, errV2AgentCarrier
	}
	clientKey, err := base64.RawURLEncoding.Strict().DecodeString(input.ClientIdentityPublicKey)
	if err != nil || len(clientKey) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(clientKey) != input.ClientIdentityPublicKey {
		return directV2DeviceLinkResponse{}, errV2AgentCarrier
	}
	proof, err := base64.RawURLEncoding.Strict().DecodeString(input.Proof)
	if err != nil || len(proof) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(proof) != input.Proof ||
		!ed25519.Verify(ed25519.PublicKey(clientKey), directV2ControllerProofTranscript(input.ControllerID, input.ClientIdentityPublicKey, input.ClientIdentityKeyVersion), proof) {
		return directV2DeviceLinkResponse{}, errV2AgentCarrier
	}
	devicePublicKey, ok := handler.state.identity.Public().(ed25519.PublicKey)
	if !ok || len(devicePublicKey) != ed25519.PublicKeySize || handler.state.KeyVersion == 0 {
		return directV2DeviceLinkResponse{}, errV2AgentCarrier
	}
	now = now.UTC().Truncate(time.Second)
	expiresAt := now.Add(directV2MaximumGrantTTL)
	grantID := uuid.NewString()
	clientThumbprint := remoteauth.PublicKeyThumbprint(ed25519.PublicKey(clientKey))
	deviceThumbprint := remoteauth.PublicKeyThumbprint(devicePublicKey)
	claims := remoteauth.DeviceLinkGrantClaims{
		Audience: remoteauth.DeviceLinkGrantAudience, GrantID: grantID, ClientID: controllerID.String(), DeviceID: handler.state.DeviceID.String(),
		RelayNodeID: handler.nodeID.String(), RelayCellID: handler.cellID.String(), TargetConnectionEpoch: handler.epoch,
		ClientIdentityKey: input.ClientIdentityPublicKey, ClientKeyThumbprint: clientThumbprint, ClientIdentityKeyVersion: input.ClientIdentityKeyVersion,
		DeviceKeyThumbprint: deviceThumbprint, DeviceIdentityKeyVersion: handler.state.KeyVersion,
		ClientGrantVersion: input.ClientIdentityKeyVersion, DeviceGrantVersion: handler.state.KeyVersion,
		AllowedScopes: append([]string(nil), handler.local.allowedScopes...), MaximumLifetimeSeconds: uint32(directV2MaximumGrantTTL / time.Second),
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: expiresAt.Unix(),
	}
	grant, err := handler.local.issuer.Sign(claims)
	if err != nil {
		return directV2DeviceLinkResponse{}, errV2AgentCarrier
	}
	encodedDeviceKey := base64.RawURLEncoding.EncodeToString(devicePublicKey)
	return directV2DeviceLinkResponse{
		Device: directV2DeviceView{
			ID: handler.state.DeviceID.String(), Name: handler.local.metadata.Name, Platform: handler.local.metadata.Platform, AgentVersion: handler.local.metadata.AgentVersion,
			IdentityAlgorithm: "Ed25519", IdentityPublicKey: encodedDeviceKey, IdentityKeyVersion: handler.state.KeyVersion, KeyThumbprint: deviceThumbprint,
		},
		DeviceLink: directV2DeviceLinkView{
			GrantID: grantID, Grant: grant, ExpiresAt: expiresAt, MaximumLifetimeSeconds: claims.MaximumLifetimeSeconds,
			ConnectionMode: "direct", ConnectionURL: handler.local.connectionURL, RelayURL: handler.local.connectionURL,
			RelayNodeID: handler.nodeID.String(), RelayCellID: handler.cellID.String(), TargetConnectionEpoch: handler.epoch,
			DeviceIdentityAlgorithm: "Ed25519", DeviceIdentityPublicKey: encodedDeviceKey, DeviceKeyThumbprint: deviceThumbprint, DeviceIdentityKeyVersion: handler.state.KeyVersion,
		},
	}, nil
}

func directV2ControllerProofTranscript(controllerID, publicKey string, version uint64) []byte {
	return []byte(directV2LocalProofDomain + "\n" + controllerID + "\n" + publicKey + "\n" + strconv.FormatUint(version, 10))
}

func writeDirectV2Problem(writer http.ResponseWriter, status int, code, title, detail string) {
	writer.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(directV2Problem{
		Type: "https://wenzwork.example/problems/" + code, Title: title, Status: status, Detail: detail, Code: code,
	})
}

func (handler *directV2Handler) authenticate(envelope *remotev2.CarrierEnvelope, hello *remotev2.CarrierHello, now time.Time) (directV2Authentication, error) {
	if handler == nil || handler.state == nil || envelope == nil || hello == nil || hello.GetGrant() == "" ||
		hello.GetDeviceConnectionTicket() != "" || hello.GetDeviceId() != "" || hello.GetDeviceConnectionEpoch() != 0 || len(hello.GetDeviceProof()) != 0 {
		return directV2Authentication{}, errV2AgentCarrier
	}
	hostClaims, hostErr := handler.authenticateWithVerifier(handler.hostVerifier, envelope, hello, now)
	var localClaims remoteauth.DeviceLinkGrantClaims
	localErr := errV2AgentCarrier
	if handler.local.enabled {
		localClaims, localErr = handler.authenticateWithVerifier(handler.local.verifier, envelope, hello, now)
	}
	if hostErr == nil && localErr != nil {
		return directV2Authentication{DeviceLinkGrantClaims: hostClaims, realm: directV2HostGrant}, nil
	}
	if localErr == nil && hostErr != nil {
		return directV2Authentication{DeviceLinkGrantClaims: localClaims, realm: directV2LocalGrant}, nil
	}
	return directV2Authentication{}, errV2AgentCarrier
}

func (handler *directV2Handler) authenticateWithVerifier(
	verifier remoteauth.DeviceLinkGrantVerifier,
	envelope *remotev2.CarrierEnvelope,
	hello *remotev2.CarrierHello,
	now time.Time,
) (remoteauth.DeviceLinkGrantClaims, error) {
	claims, err := verifier.Verify(hello.GetGrant(), now)
	if err != nil || claims.Persistent() || claims.MaximumLifetimeSeconds == 0 || claims.MaximumLifetimeSeconds > uint32(directV2MaximumGrantTTL/time.Second) ||
		claims.GrantID != hello.GetGrantId() || claims.ClientID != hello.GetClientId() || claims.DeviceID != handler.state.DeviceID.String() ||
		claims.RelayNodeID != handler.nodeID.String() || claims.RelayCellID != handler.cellID.String() || claims.TargetConnectionEpoch != handler.epoch ||
		claims.ClientIdentityKeyVersion != hello.GetClientIdentityKeyVersion() || claims.DeviceIdentityKeyVersion != handler.state.KeyVersion ||
		claims.DeviceKeyThumbprint != remoteauth.PublicKeyThumbprint(handler.state.identity.Public().(ed25519.PublicKey)) || len(hello.GetClientChallenge()) != 32 {
		return remoteauth.DeviceLinkGrantClaims{}, errV2AgentCarrier
	}
	clientKey, err := remoteauth.DecodeIdentityPublicKey(claims.ClientIdentityKey, claims.ClientKeyThumbprint)
	if err != nil {
		return remoteauth.DeviceLinkGrantClaims{}, errV2AgentCarrier
	}
	proof := remoteauth.CarrierProof{
		GrantID: claims.GrantID, CarrierID: envelope.GetCarrierId(), CarrierEpoch: envelope.GetCarrierEpoch(), Challenge: hello.GetClientChallenge(),
	}
	if remoteauth.VerifyCarrierProof(clientKey, proof, hello.GetClientProof()) != nil {
		return remoteauth.DeviceLinkGrantClaims{}, errV2AgentCarrier
	}
	return claims, nil
}

func directV2RegistryKey(realm directV2GrantRealm, clientID string) string {
	return string(realm) + "\x00" + clientID
}

func (handler *directV2Handler) bindRegistry(realm directV2GrantRealm, clientID string, carrier *v2AgentCarrier) *v2AgentLinkRegistry {
	if handler == nil || (realm != directV2HostGrant && realm != directV2LocalGrant) || clientID == "" || carrier == nil {
		return nil
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if (realm == directV2HostGrant && !handler.enabled.Load()) || (realm == directV2LocalGrant && !handler.local.enabled) {
		return nil
	}
	now := time.Now().UTC()
	for id, entry := range handler.registries {
		if entry != nil && entry.carrier == nil && now.Sub(entry.lastUsed) > v2LinkRecoveryTTL {
			entry.registry.close()
			delete(handler.registries, id)
		}
	}
	registryKey := directV2RegistryKey(realm, clientID)
	entry := handler.registries[registryKey]
	if entry == nil {
		realmEntries := 0
		for _, current := range handler.registries {
			if current != nil && current.realm == realm {
				realmEntries++
			}
		}
		if realmEntries >= v2MaximumLinksPerDevice {
			return nil
		}
		entry = &directV2RegistryEntry{registry: newDetachedV2AgentLinkRegistry(handler.state), realm: realm}
		handler.registries[registryKey] = entry
	}
	if entry.carrier != nil && entry.carrier != carrier {
		entry.carrier.close()
	}
	entry.carrier, entry.lastUsed = carrier, now
	return entry.registry
}

func (handler *directV2Handler) setEnabled(enabled bool) {
	if handler == nil {
		return
	}
	previous := handler.enabled.Swap(enabled)
	if enabled || !previous {
		return
	}
	handler.mu.Lock()
	entries := make([]*directV2RegistryEntry, 0, len(handler.registries))
	for key, entry := range handler.registries {
		if entry != nil && entry.realm == directV2HostGrant {
			entries = append(entries, entry)
			delete(handler.registries, key)
		}
	}
	handler.mu.Unlock()
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if entry.carrier != nil {
			entry.carrier.close()
		}
		entry.registry.close()
	}
}

func (handler *directV2Handler) releaseRegistry(realm directV2GrantRealm, clientID string, carrier *v2AgentCarrier) {
	if handler == nil {
		return
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if entry := handler.registries[directV2RegistryKey(realm, clientID)]; entry != nil && entry.carrier == carrier {
		entry.carrier, entry.lastUsed = nil, time.Now().UTC()
	}
}

func (handler *directV2Handler) close() {
	if handler == nil {
		return
	}
	handler.enabled.Store(false)
	handler.mu.Lock()
	entries := make([]*directV2RegistryEntry, 0, len(handler.registries))
	for _, entry := range handler.registries {
		entries = append(entries, entry)
	}
	handler.registries = make(map[string]*directV2RegistryEntry)
	handler.mu.Unlock()
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if entry.carrier != nil {
			entry.carrier.close()
		}
		entry.registry.close()
	}
}

func directV2HasOnlySubprotocol(values []string) bool {
	protocols := make([]string, 0, len(values))
	for _, value := range values {
		for _, protocol := range strings.Split(value, ",") {
			if protocol = strings.TrimSpace(protocol); protocol != "" {
				protocols = append(protocols, protocol)
			}
		}
	}
	return len(protocols) == 1 && protocols[0] == directV2Subprotocol
}
