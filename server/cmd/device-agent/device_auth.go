package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

var (
	errDeviceAuthentication = errors.New("device authentication is unavailable")
	errControlRetryable     = errors.New("control request is retryable")
)

type deviceTokenSet struct {
	AccessToken      string    `json:"access_token"`
	ExpiresIn        int64     `json:"expires_in"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresIn int64     `json:"refresh_expires_in"`
	SessionID        uuid.UUID `json:"session_id"`
	Scope            string    `json:"scope"`
}

type deviceTokenManager struct {
	mu              sync.Mutex
	client          *http.Client
	controlURL      *url.URL
	store           *controlStateStore
	now             func() time.Time
	accessToken     string
	accessExpiresAt time.Time
	refreshAt       time.Time
}

type controlHTTPError struct {
	Status     int
	Code       string
	RetryAfter time.Duration
}

func (err *controlHTTPError) Error() string {
	return fmt.Sprintf("control request rejected (HTTP %d, %s)", err.Status, err.Code)
}

func (err *controlHTTPError) Unwrap() error {
	if err.Status == http.StatusUnauthorized {
		return errDeviceAuthentication
	}
	if err.Status == http.StatusTooManyRequests || err.Status >= 500 {
		return errControlRetryable
	}
	return nil
}

func newDeviceTokenManager(client *http.Client, controlURL *url.URL, store *controlStateStore) (*deviceTokenManager, error) {
	if client == nil || controlURL == nil || store == nil {
		return nil, errors.New("device token dependencies are required")
	}
	return &deviceTokenManager{client: client, controlURL: controlURL, store: store, now: func() time.Time { return time.Now().UTC() }}, nil
}

// bootstrapOrResume uses the encrypted refresh token after first enrollment.
// When that credential has been revoked (for example, after refresh-token
// replay protection intervenes), an explicitly configured DeviceKey can
// re-authorize this same installation. The key is never persisted and is only
// sent to the Control Plane over the configured control connection.
func (manager *deviceTokenManager) bootstrapOrResume(ctx context.Context, accessKey, deviceName string) (uuid.UUID, error) {
	state, err := manager.store.snapshot()
	if err != nil {
		return uuid.Nil, err
	}
	if state.Auth.RefreshToken != "" {
		if state.Auth.RefreshExpiresAt.After(manager.now()) && state.Auth.SessionID != uuid.Nil {
			if err := manager.refresh(ctx, ""); err == nil {
				state, err = manager.store.snapshot()
				if err != nil {
					return uuid.Nil, err
				}
				return state.Auth.SessionID, nil
			} else if !errors.Is(err, errDeviceAuthentication) {
				return uuid.Nil, err
			}
		}
	}
	return manager.bootstrapWithAccessKey(ctx, accessKey, deviceName)
}

func (manager *deviceTokenManager) bootstrapWithAccessKey(ctx context.Context, accessKey, deviceName string) (uuid.UUID, error) {
	if !validDeviceKey(strings.TrimSpace(accessKey)) || strings.TrimSpace(deviceName) == "" {
		return uuid.Nil, errDeviceAuthentication
	}
	var tokens deviceTokenSet
	err := controlJSONRaw(ctx, manager.client, http.MethodPost, targetEndpoint(manager.controlURL, "/v1/device/access-key-bootstrap"),
		"DeviceKey "+strings.TrimSpace(accessKey), "", map[string]any{"deviceId": manager.store.deviceID, "deviceName": strings.TrimSpace(deviceName)}, &tokens)
	if err != nil {
		return uuid.Nil, fmt.Errorf("device bootstrap failed: %w", err)
	}
	if err := manager.accept(tokens); err != nil {
		return uuid.Nil, err
	}
	return tokens.SessionID, nil
}

func (manager *deviceTokenManager) acceptInitial(tokens deviceTokenSet) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.acceptLocked(tokens)
}

func (manager *deviceTokenManager) accept(tokens deviceTokenSet) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.acceptLocked(tokens)
}

func (manager *deviceTokenManager) acceptLocked(tokens deviceTokenSet) error {
	now := manager.now().UTC()
	if tokens.AccessToken == "" || len(tokens.AccessToken) > 8192 || tokens.RefreshToken == "" || len(tokens.RefreshToken) > 8192 ||
		tokens.ExpiresIn < 1 || tokens.RefreshExpiresIn < 1 || tokens.SessionID == uuid.Nil || !containsField(tokens.Scope, "remote.connect") {
		return errors.New("device token response is invalid")
	}
	accessExpiresAt := now.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	refreshExpiresAt := now.Add(time.Duration(tokens.RefreshExpiresIn) * time.Second)
	if !refreshExpiresAt.After(accessExpiresAt) {
		return errors.New("device token lifetime is invalid")
	}
	if err := manager.store.update(func(state *controlPersistentState) error {
		state.Auth = controlAuthState{
			RefreshToken: tokens.RefreshToken, RefreshExpiresAt: refreshExpiresAt,
			SessionID: tokens.SessionID, Scope: tokens.Scope,
		}
		return nil
	}); err != nil {
		return err
	}
	manager.accessToken = tokens.AccessToken
	manager.accessExpiresAt = accessExpiresAt
	manager.refreshAt = proactiveRefreshTime(now, accessExpiresAt)
	return nil
}

func proactiveRefreshTime(now, expiresAt time.Time) time.Time {
	ttl := expiresAt.Sub(now)
	margin := ttl / 5
	if margin > 30*time.Second {
		margin = 30 * time.Second
	}
	if margin < 100*time.Millisecond {
		margin = ttl / 2
	}
	return expiresAt.Add(-margin)
}

func (manager *deviceTokenManager) authorization(ctx context.Context) (string, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.accessToken == "" || !manager.now().Before(manager.refreshAt) {
		if err := manager.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	return "Bearer " + manager.accessToken, nil
}

func (manager *deviceTokenManager) refresh(ctx context.Context, rejectedAccessToken string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if rejectedAccessToken != "" && manager.accessToken != rejectedAccessToken && manager.accessToken != "" && manager.now().Before(manager.refreshAt) {
		return nil
	}
	return manager.refreshLocked(ctx)
}

func (manager *deviceTokenManager) refreshLocked(ctx context.Context) error {
	state, err := manager.store.snapshot()
	if err != nil {
		return err
	}
	if state.Auth.RefreshToken == "" || !state.Auth.RefreshExpiresAt.After(manager.now()) {
		return errDeviceAuthentication
	}
	values := url.Values{
		"grant_type": {auth.RefreshTokenGrantType}, "client_id": {auth.DesktopClientID}, "refresh_token": {state.Auth.RefreshToken},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, targetEndpoint(manager.controlURL, "/api/v1/oauth/token"), strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := manager.client.Do(request)
	if err != nil {
		return fmt.Errorf("refresh device session: %w", errControlRetryable)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read refresh response: %w", errControlRetryable)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var oauthProblem struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &oauthProblem)
		if oauthProblem.Error == "invalid_grant" || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusBadRequest {
			manager.accessToken = ""
			return errDeviceAuthentication
		}
		return &controlHTTPError{Status: response.StatusCode, Code: safeControlCode(oauthProblem.Error)}
	}
	var tokens deviceTokenSet
	if json.Unmarshal(body, &tokens) != nil {
		return errors.New("device refresh response is invalid")
	}
	return manager.acceptLocked(tokens)
}

func (manager *deviceTokenManager) doJSON(ctx context.Context, method, path, idempotency string, input, output any) error {
	for attempt := 0; attempt < 2; attempt++ {
		authorization, err := manager.authorization(ctx)
		if err != nil {
			return err
		}
		rejectedToken := strings.TrimPrefix(authorization, "Bearer ")
		target, targetErr := controlRequestEndpoint(manager.controlURL, path)
		if targetErr != nil {
			return targetErr
		}
		err = controlJSONRaw(ctx, manager.client, method, target, authorization, idempotency, input, output)
		var httpErr *controlHTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized || attempt != 0 {
			return err
		}
		if err := manager.refresh(ctx, rejectedToken); err != nil {
			return err
		}
	}
	return errDeviceAuthentication
}

func controlRequestEndpoint(base *url.URL, pathAndQuery string) (string, error) {
	relative, err := url.Parse(pathAndQuery)
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.User != nil || relative.Fragment != "" || !strings.HasPrefix(relative.Path, "/") {
		return "", errors.New("control request path is invalid")
	}
	copy := *base
	copy.Path = strings.TrimRight(base.Path, "/") + relative.Path
	copy.RawPath, copy.RawQuery, copy.Fragment = "", relative.RawQuery, ""
	return copy.String(), nil
}

func controlJSONRaw(ctx context.Context, client *http.Client, method, target, authorization, idempotency string, input, output any) error {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json, application/problem+json")
	request.Header.Set("Authorization", authorization)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("control request failed: %w", errControlRetryable)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("control response failed: %w", errControlRetryable)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payload, &problem)
		code := problem.Code
		if code == "" {
			code = problem.Error
		}
		retryAfter := parseControlRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
		return &controlHTTPError{Status: response.StatusCode, Code: safeControlCode(code), RetryAfter: retryAfter}
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if decoder.Decode(output) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("control response JSON is invalid")
	}
	return nil
}

func parseControlRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds >= 0 && seconds <= 3600 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	deadline, err := http.ParseTime(value)
	if err != nil || !deadline.After(now) {
		return 0
	}
	return min(time.Hour, deadline.Sub(now))
}

func safeControlCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 80 {
		return "request_rejected"
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '_' && character != '-' && character != '.' {
			return "request_rejected"
		}
	}
	return value
}
