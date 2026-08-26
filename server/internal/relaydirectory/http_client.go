package relaydirectory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

type AccessKeyClient struct {
	baseURL    string
	accessKey  string
	httpClient *http.Client
}

func NewAccessKeyClient(baseURL, accessKey string) (*AccessKeyClient, error) {
	baseURL = strings.TrimSpace(baseURL)
	accessKey = strings.TrimSpace(accessKey)
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("RELAY_MANAGEMENT_URL must be an HTTP(S) origin without credentials, query, or fragment")
	}
	encodedKey := strings.TrimPrefix(accessKey, "relay_")
	decodedKey, decodeErr := base64.RawURLEncoding.DecodeString(encodedKey)
	if !strings.HasPrefix(accessKey, "relay_") || len(encodedKey) != 43 || decodeErr != nil || len(decodedKey) != 32 {
		return nil, errors.New("RELAY_ACCESS_KEY is invalid")
	}
	return &AccessKeyClient{
		baseURL: strings.TrimRight(baseURL, "/"), accessKey: accessKey,
		httpClient: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (client *AccessKeyClient) Resolve(ctx context.Context) (relaymanagement.AccessKeyBinding, error) {
	var result relaymanagement.AccessKeyBinding
	err := client.do(ctx, http.MethodGet, "/api/v1/relay/agent/configuration", nil, http.StatusOK, &result)
	return result, err
}

func (client *AccessKeyClient) Register(ctx context.Context, input relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error) {
	var result relaymanagement.NodeInstance
	err := client.do(ctx, http.MethodPost, "/api/v1/relay/agent/instances", input, http.StatusCreated, &result)
	return result, err
}

func (client *AccessKeyClient) Heartbeat(ctx context.Context, input relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error) {
	var result relaymanagement.HeartbeatResult
	path := "/api/v1/relay/agent/instances/" + url.PathEscape(input.InstanceID.String()) + "/heartbeats"
	err := client.do(ctx, http.MethodPost, path, input, http.StatusOK, &result)
	return result, err
}

func (client *AccessKeyClient) Unregister(ctx context.Context, instanceID uuid.UUID) error {
	path := "/api/v1/relay/agent/instances/" + url.PathEscape(instanceID.String())
	return client.do(ctx, http.MethodDelete, path, nil, http.StatusNoContent, nil)
}

func (client *AccessKeyClient) Close() error {
	client.httpClient.CloseIdleConnections()
	return nil
}

func (client *AccessKeyClient) do(ctx context.Context, method, path string, requestBody any, expectedStatus int, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode Relay management request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create Relay management request: %w", err)
	}
	request.Header.Set("Authorization", "RelayKey "+client.accessKey)
	request.Header.Set("Accept", "application/json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Relay management request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		switch response.StatusCode {
		case http.StatusBadRequest:
			return relaymanagement.ErrInvalidInput
		case http.StatusUnauthorized:
			return relaymanagement.ErrAccessKeyInvalid
		case http.StatusForbidden:
			return relaymanagement.ErrInstallationRevoked
		case http.StatusNotFound:
			return relaymanagement.ErrNotFound
		case http.StatusConflict:
			return relaymanagement.ErrConflict
		default:
			return fmt.Errorf("Relay management request returned HTTP %d", response.StatusCode)
		}
	}
	if responseBody == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, (1<<20)+1))
	if err := decoder.Decode(responseBody); err != nil {
		return fmt.Errorf("decode Relay management response: %w", err)
	}
	return nil
}
