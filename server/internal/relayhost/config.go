package relayhost

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigFile      = "/etc/wenzwork-relay/config.yaml"
	DefaultIdentityFile    = "/var/lib/wenzwork-relay/identity/identity.key"
	DefaultCertificateFile = "/etc/wenzwork-relay/tls/node.crt"
	DefaultCAFile          = "/etc/wenzwork-relay/tls/ca.crt"
)

type Config struct {
	AccessKeyMode          bool              `yaml:"-" json:"-"`
	ConfigurationVersion   int64             `yaml:"-" json:"configurationVersion,omitempty"`
	InstallationID         uuid.UUID         `yaml:"installation_id" json:"installationId"`
	CellID                 uuid.UUID         `yaml:"cell_id" json:"cellId"`
	Version                string            `yaml:"version" json:"version"`
	ProtocolVersion        int               `yaml:"protocol_version" json:"protocolVersion"`
	DirectoryURL           string            `yaml:"directory_url" json:"directoryUrl"`
	PublicEndpoint         string            `yaml:"public_endpoint,omitempty" json:"publicEndpoint,omitempty"`
	BrowserOriginPatterns  []string          `yaml:"browser_origin_patterns,omitempty" json:"browserOriginPatterns,omitempty"`
	ListenAddress          string            `yaml:"listen_address" json:"listenAddress"`
	HealthAddress          string            `yaml:"health_address" json:"healthAddress"`
	IdentityPrivateKeyFile string            `yaml:"identity_private_key_file" json:"identityPrivateKeyFile"`
	CertificateFile        string            `yaml:"certificate_file" json:"certificateFile"`
	CACertificateFile      string            `yaml:"ca_certificate_file" json:"caCertificateFile"`
	RedisURL               string            `yaml:"redis_url,omitempty" json:"-"`
	TicketIssuer           string            `yaml:"ticket_issuer,omitempty" json:"ticketIssuer"`
	TicketPublicKeyFiles   map[string]string `yaml:"ticket_public_key_files,omitempty" json:"ticketPublicKeyFiles"`
	// DeviceLinkGrant verification keys are deliberately independent from device
	// connection Tickets. They authenticate only the short-lived v2
	// Client-to-Device grant carried in CARRIER_HELLO.
	DeviceLinkGrantPublicKeyFiles map[string]string `yaml:"device_link_grant_public_key_files,omitempty" json:"deviceLinkGrantPublicKeyFiles"`
	TicketPublicKeys              map[string]string `yaml:"-" json:"-"`
	DeviceLinkGrantPublicKeys     map[string]string `yaml:"-" json:"-"`
	ConnectionHardLimit           int               `yaml:"connection_hard_limit,omitempty" json:"connectionHardLimit"`
	HandshakeConcurrency          int               `yaml:"handshake_concurrency,omitempty" json:"handshakeConcurrency"`
}

func Load(path string) (Config, error) {
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Config{}, fmt.Errorf("read Relay config: %w", err)
	}
	var config Config
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode Relay config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) Validate() error {
	var problems []error
	if config.ConfigurationVersion < 0 {
		problems = append(problems, errors.New("configuration_version is invalid"))
	}
	if config.InstallationID == uuid.Nil {
		problems = append(problems, errors.New("installation_id is required"))
	}
	if config.CellID == uuid.Nil {
		problems = append(problems, errors.New("cell_id is required"))
	}
	if strings.TrimSpace(config.Version) == "" || len(config.Version) > 64 || strings.ContainsAny(config.Version, "\r\n\x00") {
		problems = append(problems, errors.New("version is invalid"))
	}
	if config.ProtocolVersion != 2 {
		problems = append(problems, errors.New("protocol_version must be 2 for remote/v2-only Relay"))
	}
	if !config.AccessKeyMode {
		directoryURL, err := url.Parse(strings.TrimSpace(config.DirectoryURL))
		if err != nil || directoryURL.Scheme != "https" || directoryURL.Host == "" || directoryURL.User != nil || directoryURL.RawQuery != "" || directoryURL.Fragment != "" {
			problems = append(problems, errors.New("directory_url must be an absolute HTTPS URL without credentials, query, or fragment"))
		}
	}
	publicEndpoint := strings.TrimSpace(config.PublicEndpoint)
	if config.AccessKeyMode && publicEndpoint == "" {
		problems = append(problems, errors.New("public_endpoint is required in Access Key mode"))
	} else if publicEndpoint != "" {
		parsed, err := url.Parse(publicEndpoint)
		if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.User != nil ||
			parsed.Path != "/v2/connect" || parsed.RawQuery != "" || parsed.Fragment != "" ||
			len(publicEndpoint) > 255 || strings.ContainsAny(publicEndpoint, "\r\n\x00") {
			problems = append(problems, errors.New("public_endpoint must be an absolute WS or WSS URL ending in /v2/connect without credentials, query, or fragment"))
		}
	}
	if !validBrowserOriginPatterns(config.BrowserOriginPatterns) {
		problems = append(problems, errors.New("browser_origin_patterns must contain at most 32 absolute HTTP or HTTPS origins without paths, credentials, queries, or fragments"))
	}
	if _, _, err := net.SplitHostPort(strings.TrimSpace(config.ListenAddress)); err != nil {
		problems = append(problems, errors.New("listen_address must be a host:port address"))
	}
	healthHost, _, healthErr := net.SplitHostPort(strings.TrimSpace(config.HealthAddress))
	healthIP := net.ParseIP(healthHost)
	if healthErr != nil || (healthHost != "localhost" && (healthIP == nil || !healthIP.IsLoopback())) {
		problems = append(problems, errors.New("health_address must be a loopback host:port address"))
	}
	if !config.AccessKeyMode {
		for name, path := range map[string]string{
			"identity_private_key_file": config.IdentityPrivateKeyFile,
			"certificate_file":          config.CertificateFile,
			"ca_certificate_file":       config.CACertificateFile,
		} {
			if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') || (!filepath.IsAbs(path) && !strings.HasPrefix(path, "/")) {
				problems = append(problems, fmt.Errorf("%s must be an absolute path", name))
			}
		}
	}
	return errors.Join(problems...)
}

func validBrowserOriginPatterns(patterns []string) bool {
	if len(patterns) > 32 {
		return false
	}
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		origin := strings.TrimSpace(pattern)
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || len(origin) > 255 ||
			strings.ContainsAny(origin, "\r\n\x00") {
			return false
		}
		canonical := parsed.Scheme + "://" + parsed.Host
		if _, exists := seen[canonical]; exists {
			return false
		}
		seen[canonical] = struct{}{}
	}
	return true
}

func (config Config) AdvertisedAddresses() []string {
	if publicEndpoint := strings.TrimSpace(config.PublicEndpoint); publicEndpoint != "" {
		return []string{publicEndpoint}
	}
	return []string{strings.TrimSpace(config.ListenAddress)}
}

func (config Config) ValidateDataPlane() error {
	var problems []error
	if config.AccessKeyMode && (len(config.TicketPublicKeyFiles) != 0 || len(config.DeviceLinkGrantPublicKeyFiles) != 0) {
		problems = append(problems, errors.New("Access Key mode requires in-memory Ticket verification keys and forbids key files"))
	}
	if err := validateServiceURL(config.RedisURL, map[string]bool{"redis": true, "rediss": true}, "redis_url"); err != nil {
		problems = append(problems, err)
	}
	issuer := strings.TrimSpace(config.TicketIssuer)
	if issuer == "" || len(issuer) > 120 || strings.ContainsAny(issuer, "\r\n\x00") {
		problems = append(problems, errors.New("ticket_issuer is invalid"))
	}
	connectionKeyCount := len(config.TicketPublicKeyFiles) + len(config.TicketPublicKeys)
	if connectionKeyCount < 1 || connectionKeyCount > 8 {
		problems = append(problems, errors.New("connection Ticket verification keys must contain 1 to 8 keys"))
	}
	connectionKeyIDs := make(map[string]struct{}, connectionKeyCount)
	for keyID, path := range config.TicketPublicKeyFiles {
		if !validTicketKeyID(keyID) {
			problems = append(problems, errors.New("ticket public Key ID is invalid"))
		}
		connectionKeyIDs[keyID] = struct{}{}
		if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') || (!filepath.IsAbs(path) && !strings.HasPrefix(path, "/")) {
			problems = append(problems, fmt.Errorf("ticket public key %q must use an absolute path", keyID))
		}
	}
	for keyID, encoded := range config.TicketPublicKeys {
		if !validTicketKeyID(keyID) || !validEncodedPublicKey(encoded) {
			problems = append(problems, errors.New("ticket public key is invalid"))
		}
		if _, reused := connectionKeyIDs[keyID]; reused {
			problems = append(problems, fmt.Errorf("ticket public Key ID %q is duplicated", keyID))
		}
		connectionKeyIDs[keyID] = struct{}{}
	}
	deviceLinkKeyCount := len(config.DeviceLinkGrantPublicKeyFiles) + len(config.DeviceLinkGrantPublicKeys)
	if deviceLinkKeyCount < 1 || deviceLinkKeyCount > 8 {
		problems = append(problems, errors.New("device link grant verification keys must contain 1 to 8 keys"))
	}
	deviceLinkKeyIDs := make(map[string]struct{}, deviceLinkKeyCount)
	for keyID, path := range config.DeviceLinkGrantPublicKeyFiles {
		if !validTicketKeyID(keyID) {
			problems = append(problems, errors.New("device link grant public Key ID is invalid"))
		}
		if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') || (!filepath.IsAbs(path) && !strings.HasPrefix(path, "/")) {
			problems = append(problems, fmt.Errorf("device link grant public key %q must use an absolute path", keyID))
		}
		if _, reused := connectionKeyIDs[keyID]; reused {
			problems = append(problems, fmt.Errorf("device link grant public Key ID %q must be independent from Ticket keys", keyID))
		}
		for _, connectionPath := range config.TicketPublicKeyFiles {
			if filepath.Clean(path) == filepath.Clean(connectionPath) {
				problems = append(problems, fmt.Errorf("device link grant public key %q must use an independent file", keyID))
			}
		}
		deviceLinkKeyIDs[keyID] = struct{}{}
	}
	for keyID, encoded := range config.DeviceLinkGrantPublicKeys {
		if !validTicketKeyID(keyID) || !validEncodedPublicKey(encoded) {
			problems = append(problems, errors.New("device link grant public key is invalid"))
		}
		if _, duplicated := deviceLinkKeyIDs[keyID]; duplicated {
			problems = append(problems, fmt.Errorf("device link grant public Key ID %q is duplicated", keyID))
		}
		if _, reused := connectionKeyIDs[keyID]; reused {
			problems = append(problems, fmt.Errorf("device link grant public Key ID %q must be independent from Ticket keys", keyID))
		}
		for _, connectionKey := range config.TicketPublicKeys {
			if encoded == connectionKey {
				problems = append(problems, fmt.Errorf("device link grant public key %q must be independent", keyID))
			}
		}
		deviceLinkKeyIDs[keyID] = struct{}{}
	}
	if config.ConnectionHardLimit != 0 && (config.ConnectionHardLimit < 1 || config.ConnectionHardLimit > 10_000_000) {
		problems = append(problems, errors.New("connection_hard_limit is invalid"))
	}
	if config.HandshakeConcurrency != 0 && (config.HandshakeConcurrency < 1 || config.HandshakeConcurrency > 100_000) {
		problems = append(problems, errors.New("handshake_concurrency is invalid"))
	}
	return errors.Join(problems...)
}

func (config Config) DataPlaneConfigured() bool {
	return strings.TrimSpace(config.RedisURL) != "" ||
		strings.TrimSpace(config.TicketIssuer) != "" || len(config.TicketPublicKeyFiles) != 0 || len(config.TicketPublicKeys) != 0 ||
		len(config.DeviceLinkGrantPublicKeyFiles) != 0 || len(config.DeviceLinkGrantPublicKeys) != 0 ||
		config.ConnectionHardLimit != 0 || config.HandshakeConcurrency != 0
}

func validTicketKeyID(keyID string) bool {
	return len(keyID) >= 1 && len(keyID) <= 120 && !strings.ContainsAny(keyID, ".*> \t\r\n\x00")
}

func validEncodedPublicKey(encoded string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	return err == nil && len(decoded) == 32
}

func (config Config) EffectiveConnectionHardLimit() int {
	if config.ConnectionHardLimit > 0 {
		return config.ConnectionHardLimit
	}
	return 10_000
}

func (config Config) EffectiveHandshakeConcurrency() int {
	if config.HandshakeConcurrency > 0 {
		return config.HandshakeConcurrency
	}
	return 128
}

func validateServiceURL(raw string, schemes map[string]bool, field string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil {
		return fmt.Errorf("%s is invalid", field)
	}
	pathValid := parsed.Path == "" || parsed.Path == "/"
	if field == "redis_url" && len(parsed.Path) > 1 && parsed.Path[0] == '/' {
		pathValid = true
		for _, character := range parsed.Path[1:] {
			if character < '0' || character > '9' {
				pathValid = false
				break
			}
		}
	}
	if !schemes[parsed.Scheme] || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || !pathValid {
		return fmt.Errorf("%s is invalid", field)
	}
	return nil
}

func Save(path string, config Config) error {
	if config.AccessKeyMode {
		return errors.New("Access Key runtime configuration is memory-only and cannot be persisted")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return errors.New("Relay config path is required")
	}
	contents, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode Relay config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create Relay config directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create Relay config temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure Relay config temporary file: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write Relay config temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync Relay config temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Relay config temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install Relay config: %w", err)
	}
	return nil
}

func WriteCredential(path string, contents []byte, mode os.FileMode) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" || len(contents) == 0 {
		return errors.New("Relay credential path and content are required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create Relay credential directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return fmt.Errorf("create Relay credential temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install Relay credential: %w", err)
	}
	return os.Chmod(path, mode)
}
