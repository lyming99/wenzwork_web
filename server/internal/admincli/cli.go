package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maximumInputBytes = 2 << 20

type usageError struct {
	message string
}

func (e usageError) Error() string { return e.message }

func IsUsageError(err error) bool {
	var target usageError
	return errors.As(err, &target)
}

type environment func(string) string

type client struct {
	baseURL *url.URL
	origin  string
	http    *http.Client
}

// Run executes a single non-interactive administrator command. Authentication
// secrets are read only from the process environment and are never printed.
func Run(ctx context.Context, args []string, output io.Writer, getenv environment) error {
	if len(args) < 2 {
		return usageError{message: usage()}
	}
	if output == nil {
		output = io.Discard
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	c, err := newClient(getenv)
	if err != nil {
		return err
	}
	if err := c.authenticate(ctx, getenv); err != nil {
		return err
	}

	resource := strings.ToLower(strings.TrimSpace(args[0]))
	switch resource {
	case "docs", "documents":
		return runDocuments(ctx, c, args[1:], output)
	case "releases", "release":
		return runReleases(ctx, c, args[1:], output)
	default:
		return usageError{message: usage()}
	}
}

func usage() string {
	return strings.TrimSpace(`usage:
  go run ./cmd/admin docs list [--status draft|published|archived] [--query text]
  go run ./cmd/admin docs get <document-id>
  go run ./cmd/admin docs create --file document.json
  go run ./cmd/admin docs update <document-id> --file document.json
  go run ./cmd/admin docs publish <document-id> --confirm
  go run ./cmd/admin docs archive <document-id> --confirm
  go run ./cmd/admin releases draft --file release.json
  go run ./cmd/admin releases publish <release-id> --confirm

environment:
  WENZWORK_ADMIN_API_URL   API base URL (default http://localhost:8080/api/v1)
  WENZWORK_ADMIN_EMAIL     administrator email
  WENZWORK_ADMIN_PASSWORD  administrator password
  WENZWORK_ADMIN_MFA_CODE  current TOTP or unused recovery code

For an existing automation session, WENZWORK_ADMIN_SESSION and
WENZWORK_ADMIN_CSRF may replace email/password/MFA authentication.`)
}

func newClient(getenv environment) (*client, error) {
	raw := strings.TrimSpace(getenv("WENZWORK_ADMIN_API_URL"))
	if raw == "" {
		raw = "http://localhost:8080/api/v1"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("WENZWORK_ADMIN_API_URL must be an absolute http(s) URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery, parsed.Fragment = "", ""
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create administrator cookie jar: %w", err)
	}
	return &client{
		baseURL: parsed,
		origin:  parsed.Scheme + "://" + parsed.Host,
		http:    &http.Client{Jar: jar, Timeout: 20 * time.Second},
	}, nil
}

func (c *client) authenticate(ctx context.Context, getenv environment) error {
	if session, csrf := strings.TrimSpace(getenv("WENZWORK_ADMIN_SESSION")), strings.TrimSpace(getenv("WENZWORK_ADMIN_CSRF")); session != "" || csrf != "" {
		if session == "" || csrf == "" {
			return errors.New("WENZWORK_ADMIN_SESSION and WENZWORK_ADMIN_CSRF must be provided together")
		}
		secure := c.baseURL.Scheme == "https"
		prefix := "wenzwork_"
		if secure {
			prefix = "__Host-wenzwork_"
		}
		c.http.Jar.SetCookies(c.baseURL, []*http.Cookie{
			{Name: prefix + "session", Value: session, Path: "/", Secure: secure, HttpOnly: true},
			{Name: prefix + "csrf", Value: csrf, Path: "/", Secure: secure},
		})
		return nil
	}

	email := strings.TrimSpace(getenv("WENZWORK_ADMIN_EMAIL"))
	password := getenv("WENZWORK_ADMIN_PASSWORD")
	if email == "" || password == "" {
		return errors.New("WENZWORK_ADMIN_EMAIL and WENZWORK_ADMIN_PASSWORD are required")
	}
	var login struct {
		MFARequired    bool  `json:"mfaRequired"`
		MFAEnforced    *bool `json:"mfaEnforced"`
		AssuranceLevel int   `json:"assuranceLevel"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/auth/login", map[string]any{
		"email": email, "password": password, "rememberMe": false,
	}, &login); err != nil {
		return fmt.Errorf("administrator login failed: %w", err)
	}
	if login.AssuranceLevel >= 2 {
		return nil
	}
	if login.MFAEnforced != nil && !*login.MFAEnforced {
		return nil
	}
	if !login.MFARequired {
		return errors.New("administrator MFA is not enrolled; enroll TOTP in the account security page")
	}
	code := strings.TrimSpace(getenv("WENZWORK_ADMIN_MFA_CODE"))
	if code == "" {
		return errors.New("WENZWORK_ADMIN_MFA_CODE is required for administrator login")
	}
	var verified struct {
		AssuranceLevel int `json:"assuranceLevel"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/auth/mfa/totp/verify", map[string]string{"code": code}, &verified); err != nil {
		return fmt.Errorf("administrator MFA verification failed: %w", err)
	}
	if verified.AssuranceLevel < 2 {
		return errors.New("administrator session did not reach MFA assurance level 2")
	}
	return nil
}

func runDocuments(ctx context.Context, c *client, args []string, output io.Writer) error {
	if len(args) == 0 {
		return usageError{message: usage()}
	}
	switch args[0] {
	case "list":
		flags := flag.NewFlagSet("docs list", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		status := flags.String("status", "", "document status")
		query := flags.String("query", "", "search text")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return usageError{message: "invalid docs list arguments\n\n" + usage()}
		}
		values := url.Values{}
		if value := strings.TrimSpace(*status); value != "" {
			if value != "draft" && value != "published" && value != "archived" {
				return usageError{message: "--status must be draft, published, or archived"}
			}
			values.Set("status", value)
		}
		if value := strings.TrimSpace(*query); value != "" {
			values.Set("q", value)
		}
		path := "/admin/help-documents?limit=100"
		if encoded := values.Encode(); encoded != "" {
			path += "&" + encoded
		}
		return c.requestAndPrint(ctx, http.MethodGet, path, nil, output)
	case "get":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return usageError{message: "docs get requires a document id"}
		}
		return c.requestAndPrint(ctx, http.MethodGet, "/admin/help-documents/"+url.PathEscape(args[1]), nil, output)
	case "create":
		file, err := parseFileFlag("docs create", args[1:])
		if err != nil {
			return err
		}
		payload, err := readJSONFile(file)
		if err != nil {
			return err
		}
		return c.requestAndPrint(ctx, http.MethodPost, "/admin/help-documents", payload, output)
	case "update":
		if len(args) < 2 {
			return usageError{message: "docs update requires a document id and --file"}
		}
		file, err := parseFileFlag("docs update", args[2:])
		if err != nil {
			return err
		}
		payload, err := readJSONFile(file)
		if err != nil {
			return err
		}
		return c.requestAndPrint(ctx, http.MethodPut, "/admin/help-documents/"+url.PathEscape(args[1]), payload, output)
	case "publish", "archive":
		if len(args) != 3 || args[2] != "--confirm" {
			return usageError{message: fmt.Sprintf("docs %s requires <document-id> --confirm", args[0])}
		}
		method, path := http.MethodPost, "/admin/help-documents/"+url.PathEscape(args[1])+"/publish"
		if args[0] == "archive" {
			method, path = http.MethodDelete, "/admin/help-documents/"+url.PathEscape(args[1])
		}
		return c.requestAndPrint(ctx, method, path, nil, output)
	default:
		return usageError{message: usage()}
	}
}

func runReleases(ctx context.Context, c *client, args []string, output io.Writer) error {
	if len(args) == 0 {
		return usageError{message: usage()}
	}
	switch args[0] {
	case "draft":
		file, err := parseFileFlag("releases draft", args[1:])
		if err != nil {
			return err
		}
		payload, err := readJSONObject(file)
		if err != nil {
			return err
		}
		payload["status"] = "draft"
		return c.requestAndPrint(ctx, http.MethodPost, "/admin/releases", payload, output)
	case "publish":
		if len(args) != 3 || args[2] != "--confirm" {
			return usageError{message: "releases publish requires <release-id> --confirm"}
		}
		return c.requestAndPrint(ctx, http.MethodPost, "/admin/releases/"+url.PathEscape(args[1])+"/publish", nil, output)
	default:
		return usageError{message: usage()}
	}
}

func parseFileFlag(name string, args []string) (string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("file", "", "JSON input file")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*file) == "" {
		return "", usageError{message: name + " requires --file <json-file>"}
	}
	return *file, nil
}

func readJSONFile(path string) (json.RawMessage, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("open JSON input: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read JSON input: %w", err)
	}
	if len(data) > maximumInputBytes {
		return nil, fmt.Errorf("JSON input exceeds %d bytes", maximumInputBytes)
	}
	if !json.Valid(data) {
		return nil, errors.New("input file does not contain valid JSON")
	}
	return json.RawMessage(data), nil
}

func readJSONObject(path string) (map[string]any, error) {
	raw, err := readJSONFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, errors.New("input file must contain one JSON object")
	}
	return value, nil
}

func (c *client) requestAndPrint(ctx context.Context, method, path string, body any, output io.Writer) error {
	var result json.RawMessage
	if err := c.doJSON(ctx, method, path, body, &result); err != nil {
		return err
	}
	if len(result) == 0 {
		_, err := fmt.Fprintln(output, "ok")
		return err
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, result, "", "  "); err != nil {
		return fmt.Errorf("format API response: %w", err)
	}
	formatted.WriteByte('\n')
	_, err := output.Write(formatted.Bytes())
	return err
}

func (c *client) doJSON(ctx context.Context, method, path string, body any, output any) error {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(c.baseURL.Path, "/") + "/" + strings.TrimLeft(path, "/")
	endpoint.RawQuery = ""
	if queryIndex := strings.IndexByte(endpoint.Path, '?'); queryIndex >= 0 {
		endpoint.RawQuery = endpoint.Path[queryIndex+1:]
		endpoint.Path = endpoint.Path[:queryIndex]
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), payload)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", c.origin)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		if token := c.csrfToken(); token != "" {
			request.Header.Set("X-CSRF-Token", token)
		}
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call administrator API: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumInputBytes+1))
	if err != nil {
		return fmt.Errorf("read administrator API response: %w", err)
	}
	if len(data) > maximumInputBytes {
		return errors.New("administrator API response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
			Code   string `json:"code"`
		}
		if json.Unmarshal(data, &problem) == nil && (problem.Title != "" || problem.Detail != "") {
			message := strings.TrimSpace(problem.Title + ": " + problem.Detail)
			if problem.Code != "" {
				message += " (" + problem.Code + ")"
			}
			return fmt.Errorf("API returned %s: %s", response.Status, message)
		}
		return fmt.Errorf("API returned %s", response.Status)
	}
	if output == nil || len(data) == 0 {
		return nil
	}
	if raw, ok := output.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode administrator API response: %w", err)
	}
	return nil
}

func (c *client) csrfToken() string {
	for _, cookie := range c.http.Jar.Cookies(c.baseURL) {
		if cookie.Name == "wenzwork_csrf" || cookie.Name == "__Host-wenzwork_csrf" {
			return cookie.Value
		}
	}
	return ""
}
