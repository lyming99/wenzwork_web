package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/remotedevice"
)

var version = "dev"

type stageError struct {
	code int
	err  error
}

func (err stageError) Error() string { return err.err.Error() }
func (err stageError) Unwrap() error { return err.err }

type options struct {
	mode              string
	controlURL        *url.URL
	stateFile         string
	pingCount         int
	accessToken       string
	targetStateFile   string
	targetAccessToken string
	messageCount      int
	tlsCAFile         string
	jsonOutput        bool
	verbose           bool
	timeout           time.Duration
}

type reporter struct {
	stdout, stderr io.Writer
	json, verbose  bool
}

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 1 && (arguments[0] == "version" || arguments[0] == "--version") {
		fmt.Fprintln(stdout, version)
		return 0
	}
	opts, err := parseOptions(arguments, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "relay-client-test:", err)
		return 2
	}
	report := reporter{stdout: stdout, stderr: stderr, json: opts.jsonOutput, verbose: opts.verbose}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	if err := execute(ctx, opts, report); err != nil {
		code := 2
		var staged stageError
		if errors.As(err, &staged) {
			code = staged.code
		}
		if opts.jsonOutput {
			_ = json.NewEncoder(stdout).Encode(map[string]any{"event": "result", "result": "FAIL", "exitCode": code, "error": err.Error()})
		} else {
			fmt.Fprintln(stderr, "RESULT: FAIL -", err)
		}
		return code
	}
	return 0
}

func parseOptions(arguments []string, stderr io.Writer) (options, error) {
	if len(arguments) == 0 || (arguments[0] != "run" && arguments[0] != "peer") {
		return options{}, errors.New("usage: relay-client-test <run|peer> --control-url https://control.example.com --state-file <path> [options]")
	}
	mode := arguments[0]
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(stderr)
	controlRaw := flags.String("control-url", "", "Control Plane base URL")
	stateFile := flags.String("state-file", "", "local device identity state file")
	pingCount := flags.Int("ping-count", 5, "number of Ping/Pong checks (minimum 5)")
	accessToken := flags.String("access-token", "", "App Access Token for automation (never logged)")
	targetStateFile := flags.String("target-state-file", "", "peer mode target device identity state file")
	targetAccessToken := flags.String("target-access-token", "", "peer mode target App Access Token (never logged)")
	messageCount := flags.Int("message-count", 100, "peer mode messages in each direction (100-1000)")
	tlsCAFile := flags.String("tls-ca-file", "", "additional trusted CA PEM file")
	jsonOutput := flags.Bool("json", false, "write newline-delimited JSON events")
	verbose := flags.Bool("verbose", false, "show non-sensitive stage details")
	timeout := flags.Duration("timeout", 2*time.Minute, "whole-flow timeout")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
		return options{}, errors.New("invalid command arguments")
	}
	controlURL, err := validateControlURL(*controlRaw)
	if err != nil {
		return options{}, err
	}
	if strings.TrimSpace(*stateFile) == "" || *timeout < 10*time.Second || *timeout > 30*time.Minute {
		return options{}, errors.New("--state-file or --timeout (10s-30m) is invalid")
	}
	if mode == "run" && (*pingCount < 5 || *pingCount > 100) {
		return options{}, errors.New("--state-file, --ping-count (5-100), or --timeout (10s-30m) is invalid")
	}
	if mode == "peer" && (strings.TrimSpace(*targetStateFile) == "" || filepath.Clean(*targetStateFile) == filepath.Clean(*stateFile) || *messageCount < 100 || *messageCount > 1000) {
		return options{}, errors.New("peer mode requires distinct --state-file/--target-state-file and --message-count 100-1000")
	}
	return options{
		mode: mode, controlURL: controlURL, stateFile: *stateFile, pingCount: *pingCount,
		accessToken: strings.TrimSpace(*accessToken), tlsCAFile: strings.TrimSpace(*tlsCAFile),
		targetStateFile: strings.TrimSpace(*targetStateFile), targetAccessToken: strings.TrimSpace(*targetAccessToken), messageCount: *messageCount,
		jsonOutput: *jsonOutput, verbose: *verbose, timeout: *timeout,
	}, nil
}

func execute(ctx context.Context, opts options, report reporter) error {
	if opts.mode == "peer" {
		return executePeer(ctx, opts, report)
	}
	state, err := loadOrCreateState(opts.stateFile)
	if err != nil {
		return stageError{code: 2, err: err}
	}
	httpClient, err := secureHTTPClient(opts.tlsCAFile)
	if err != nil {
		return stageError{code: 2, err: err}
	}
	deviceName, _ := os.Hostname()
	if strings.TrimSpace(deviceName) == "" {
		deviceName = "WenzWork test client"
	}
	accessToken := opts.accessToken
	if accessToken == "" && state.RefreshToken != "" && state.SessionID != uuid.Nil {
		refreshed, refreshErr := refreshAccessToken(ctx, httpClient, opts.controlURL, state.RefreshToken)
		if refreshErr == nil && slices.Contains(strings.Fields(refreshed.Scope), "remote.connect") {
			accessToken, state.RefreshToken, state.SessionID = refreshed.AccessToken, refreshed.RefreshToken, refreshed.SessionID
		}
	}
	if accessToken == "" {
		tokens, oauthErr := deviceOAuth(ctx, httpClient, opts.controlURL, state.DeviceID, deviceName, report)
		if oauthErr != nil {
			return stageError{code: 10, err: oauthErr}
		}
		accessToken, state.RefreshToken, state.SessionID = tokens.AccessToken, tokens.RefreshToken, tokens.SessionID
	}
	if state.SessionID == uuid.Nil {
		return stageError{code: 10, err: errors.New("--access-token requires a state file containing sessionId")}
	}
	report.step(1, "OAuth approved", map[string]any{"deviceSuffix": shortDeviceID(state.DeviceID)})

	publicKey := state.identity.Public().(ed25519.PublicKey)
	proof, err := remotedevice.SignRegistration(state.identity, state.SessionID, state.DeviceID)
	if err != nil {
		return stageError{code: 20, err: err}
	}
	registration, err := registerDevice(ctx, httpClient, opts.controlURL, accessToken, state, deviceName, publicKey, proof)
	if err != nil {
		return stageError{code: 20, err: err}
	}
	report.step(2, "Device registered", map[string]any{"thumbprint": registration.PublicKeyThumbprint})

	if state.ConnectionEpoch == ^uint64(0) {
		return stageError{code: 2, err: errors.New("connectionEpoch is exhausted")}
	}
	state.ConnectionEpoch++
	if err := writeState(opts.stateFile, state); err != nil {
		return stageError{code: 2, err: err}
	}
	allocation, err := createAllocation(ctx, httpClient, opts.controlURL, accessToken, state)
	if err != nil {
		return stageError{code: 30, err: err}
	}
	ttl := time.Until(allocation.TicketExpiresAt).Round(time.Second)
	report.step(3, "Allocation ready", map[string]any{"cell": allocation.Primary.CellID, "ticketTTL": ttl.String()})

	result, err := connectRelay(ctx, httpClient, allocation, state, opts.pingCount, report)
	if err != nil {
		return err
	}
	report.step(5, fmt.Sprintf("Pong %d/%d", len(result.RTTs), opts.pingCount), map[string]any{
		"minMs": result.Min.Milliseconds(), "avgMs": result.Average.Milliseconds(), "maxMs": result.Max.Milliseconds(),
	})
	if opts.jsonOutput {
		_ = json.NewEncoder(report.stdout).Encode(map[string]any{
			"event": "result", "result": "PASS", "pongs": len(result.RTTs),
			"minMs": result.Min.Milliseconds(), "avgMs": result.Average.Milliseconds(), "maxMs": result.Max.Milliseconds(),
		})
	} else {
		fmt.Fprintln(report.stdout, "RESULT: PASS")
	}
	return nil
}

func (report reporter) step(number int, message string, fields map[string]any) {
	if report.json {
		event := map[string]any{"event": "stage", "step": number, "total": 5, "message": message}
		for key, value := range fields {
			event[key] = value
		}
		_ = json.NewEncoder(report.stdout).Encode(event)
		return
	}
	extra := ""
	for _, key := range []string{"thumbprint", "cell", "ticketTTL", "protocol", "heartbeatSeconds", "minMs", "avgMs", "maxMs", "ticketMs", "relayMs", "peerOpenMs", "coldStartMs", "sourceToTargetP95Ms", "targetToSourceP95Ms"} {
		if value, ok := fields[key]; ok {
			extra += fmt.Sprintf("  %s=%v", key, value)
		}
	}
	fmt.Fprintf(report.stdout, "[%d/5] %-20s%s\n", number, message, extra)
}

func (report reporter) authorization(uri, userCode string) {
	if report.json {
		_ = json.NewEncoder(report.stdout).Encode(map[string]any{"event": "oauth_authorization_required", "verificationUriComplete": uri, "userCode": userCode})
		return
	}
	fmt.Fprintf(report.stdout, "Open %s\nUser code: %s\n", uri, userCode)
}

func (report reporter) debug(message string) {
	if report.verbose {
		fmt.Fprintln(report.stderr, message)
	}
}

func validateControlURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("--control-url must be an absolute HTTPS URL")
	}
	host, _, splitErr := net.SplitHostPort(parsed.Host)
	if splitErr != nil {
		host = parsed.Hostname()
	}
	loopback := strings.EqualFold(host, "localhost")
	if address := net.ParseIP(host); address != nil {
		loopback = address.IsLoopback()
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return nil, errors.New("--control-url must use HTTPS (HTTP is allowed only on loopback)")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func secureHTTPClient(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		contents, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read --tls-ca-file: %w", err)
		}
		if !pool.AppendCertsFromPEM(contents) {
			return nil, errors.New("--tls-ca-file does not contain a certificate")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return &http.Client{
		Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type oauthTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	SessionID    uuid.UUID `json:"session_id"`
	Scope        string    `json:"scope"`
}

type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int    `json:"interval"`
}

func deviceOAuth(ctx context.Context, client *http.Client, base *url.URL, deviceID uuid.UUID, deviceName string, report reporter) (oauthTokens, error) {
	var authorization deviceAuthorization
	err := doJSON(ctx, client, http.MethodPost, endpointURL(base, "/api/v1/oauth/device-authorization"), "", "", map[string]any{
		"client_id": auth.DesktopClientID, "device_id": deviceID, "device_name": deviceName,
	}, &authorization)
	if err != nil {
		return oauthTokens{}, err
	}
	if authorization.DeviceCode == "" || authorization.UserCode == "" || authorization.VerificationURIComplete == "" || authorization.ExpiresIn < 1 || authorization.Interval < 1 {
		return oauthTokens{}, errors.New("OAuth authorization response is invalid")
	}
	report.authorization(authorization.VerificationURIComplete, authorization.UserCode)
	deadline := time.Now().Add(time.Duration(authorization.ExpiresIn) * time.Second)
	interval := time.Duration(authorization.Interval) * time.Second
	for {
		if !time.Now().Before(deadline) {
			return oauthTokens{}, errors.New("OAuth approval expired")
		}
		select {
		case <-ctx.Done():
			return oauthTokens{}, ctx.Err()
		case <-time.After(interval):
		}
		values := url.Values{
			"grant_type": {auth.DeviceGrantType}, "client_id": {auth.DesktopClientID}, "device_code": {authorization.DeviceCode},
		}
		tokens, code, err := exchangeToken(ctx, client, base, values)
		if err == nil {
			if !slices.Contains(strings.Fields(tokens.Scope), "remote.connect") {
				return oauthTokens{}, errors.New("OAuth token is missing remote.connect")
			}
			return tokens, nil
		}
		switch code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		default:
			return oauthTokens{}, err
		}
	}
}

func refreshAccessToken(ctx context.Context, client *http.Client, base *url.URL, refreshToken string) (oauthTokens, error) {
	returnToken, _, err := exchangeToken(ctx, client, base, url.Values{
		"grant_type": {auth.RefreshTokenGrantType}, "client_id": {auth.DesktopClientID}, "refresh_token": {refreshToken},
	})
	return returnToken, err
}

func exchangeToken(ctx context.Context, client *http.Client, base *url.URL, values url.Values) (oauthTokens, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(base, "/api/v1/oauth/token"), strings.NewReader(values.Encode()))
	if err != nil {
		return oauthTokens{}, "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return oauthTokens{}, "", fmt.Errorf("OAuth token request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return oauthTokens{}, "", errors.New("OAuth token response could not be read")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct{ Error, ErrorDescription string }
		_ = json.Unmarshal(body, &problem)
		if problem.Error == "" {
			problem.Error = "oauth_failed"
		}
		return oauthTokens{}, problem.Error, fmt.Errorf("OAuth token request failed (%s)", problem.Error)
	}
	var tokens oauthTokens
	if json.Unmarshal(body, &tokens) != nil || tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.SessionID == uuid.Nil {
		return oauthTokens{}, "", errors.New("OAuth token response is invalid")
	}
	return tokens, "", nil
}

type registrationResponse struct {
	PublicKeyThumbprint string `json:"publicKeyThumbprint"`
}

func registerDevice(ctx context.Context, client *http.Client, base *url.URL, accessToken string, state clientState, deviceName string, publicKey ed25519.PublicKey, proof string) (registrationResponse, error) {
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	var response registrationResponse
	err := doJSON(ctx, client, http.MethodPost, endpointURL(base, "/v1/device/registrations"), accessToken, "registration-"+uuid.NewString(), map[string]any{
		"deviceName": deviceName, "platform": platform, "agentVersion": version,
		"protocolMin": 1, "protocolMax": 1, "capabilities": []string{"relay.ping"},
		"identityAlgorithm": "ed25519", "identityPublicKey": base64.RawURLEncoding.EncodeToString(publicKey), "proof": proof,
	}, &response)
	if err != nil {
		return registrationResponse{}, err
	}
	if response.PublicKeyThumbprint == "" {
		return registrationResponse{}, errors.New("device registration response is invalid")
	}
	return response, nil
}

type allocationEndpoint struct {
	CellID           uuid.UUID `json:"cellId"`
	EndpointRevision uint64    `json:"endpointRevision"`
	URL              string    `json:"url"`
}

type allocationResponse struct {
	AssignmentID             uuid.UUID            `json:"assignmentId"`
	AssignmentVersion        uint64               `json:"assignmentVersion"`
	Primary                  allocationEndpoint   `json:"primary"`
	Fallbacks                []allocationEndpoint `json:"fallbacks"`
	ConnectionTicket         string               `json:"connectionTicket"`
	TicketExpiresAt          time.Time            `json:"ticketExpiresAt"`
	AssignmentLeaseExpiresAt time.Time            `json:"assignmentLeaseExpiresAt"`
	RefreshAfter             time.Time            `json:"refreshAfter"`
}

func createAllocation(ctx context.Context, client *http.Client, base *url.URL, accessToken string, state clientState) (allocationResponse, error) {
	var response allocationResponse
	err := doJSON(ctx, client, http.MethodPost, endpointURL(base, "/v1/device/relay-allocations"), accessToken, "allocation-"+uuid.NewString(), map[string]any{
		"remoteDeviceId": state.DeviceID, "protocolMin": 1, "protocolMax": 1, "connectionEpoch": state.ConnectionEpoch,
	}, &response)
	if err != nil {
		return allocationResponse{}, err
	}
	if response.AssignmentID == uuid.Nil || response.AssignmentVersion == 0 || response.Primary.CellID == uuid.Nil || response.ConnectionTicket == "" || !response.TicketExpiresAt.After(time.Now()) {
		return allocationResponse{}, errors.New("Relay allocation response is invalid")
	}
	return response, nil
}

func doJSON(ctx context.Context, client *http.Client, method, target, accessToken, idempotencyKey string, requestBody, destination any) error {
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, application/problem+json")
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return errors.New("response could not be read")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &problem)
		if problem.Code == "" {
			problem.Code = "http_" + strconv.Itoa(response.StatusCode)
		}
		return fmt.Errorf("request rejected (%s)", problem.Code)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		return errors.New("response JSON is invalid")
	}
	return nil
}

func endpointURL(base *url.URL, path string) string {
	copy := *base
	copy.Path = strings.TrimRight(base.Path, "/") + path
	copy.RawPath, copy.RawQuery, copy.Fragment = "", "", ""
	return copy.String()
}

func shortDeviceID(id uuid.UUID) string {
	value := strings.ReplaceAll(id.String(), "-", "")
	if len(value) <= 8 {
		return value
	}
	return value[len(value)-8:]
}
