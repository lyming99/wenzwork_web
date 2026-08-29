package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const minimumSecretLength = 32

type Config struct {
	Environment                        string
	EnvironmentFile                    string
	MigrationsDir                      string
	SystemSetupCompleted               bool
	PublicBaseURL                      string
	HTTPAddr                           string
	WebRoot                            string
	LogLevel                           string
	DatabaseURL                        string
	CookieSecure                       bool
	AdminMFARequired                   bool
	RegistrationEnabled                bool
	AllowedOrigins                     []string
	TrustedProxyCIDRs                  []string
	GeoIPCityDatabasePath              string
	IPGeolocationAPIEnabled            bool
	Argon2MemoryKiB                    uint32
	Argon2Iterations                   uint32
	Argon2Parallelism                  uint8
	SMTPHost                           string
	SMTPPort                           int
	SMTPUser                           string
	SMTPPassword                       string
	MailFrom                           string
	S3Endpoint                         string
	S3Region                           string
	S3Bucket                           string
	S3AccessKeyID                      string
	S3SecretAccessKey                  string
	S3SessionToken                     string
	S3AddressingStyle                  string
	DownloadCDNBaseURL                 string
	ReleaseAssetCacheDir               string
	ReleasePushStorageDir              string
	ReleaseAccessKey                   string
	GitHubReleaseRepository            string
	GitHubReleaseToken                 string
	DesktopGitHubReleaseRepository     string
	MobileGitHubReleaseRepository      string
	MFAEncryptionKey                   string
	RedemptionCodeHMACKey              string
	RelayCACertificateFile             string
	RelayCAPrivateKeyFile              string
	RelayDevelopmentCADir              string
	RelayDirectoryURL                  string
	RelayBootstrapAssetsDir            string
	RemoteMVPEnabled                   bool
	RemoteMVPRegion                    string
	RemoteMVPPool                      string
	RemoteMVPCell                      string
	RedisURL                           string
	RelayTicketIssuer                  string
	RelayTicketKeyID                   string
	RelayTicketPrivateKeyFile          string
	RelayTicketTTL                     time.Duration
	RelayDeviceLinkGrantKeyID          string
	RelayDeviceLinkGrantPrivateKeyFile string
	RelayDeviceLinkGrantTTL            time.Duration
}

type lookupFunc func(string) (string, bool)

func Load() (Config, error) {
	lookup, err := withGeneratedHostSecrets(os.LookupEnv)
	if err != nil {
		return Config{}, fmt.Errorf("initialize Host secrets: %w", err)
	}
	lookup, err = withGeneratedReleaseAccessKey(lookup)
	if err != nil {
		return Config{}, fmt.Errorf("initialize Release Access Key: %w", err)
	}
	return load(lookup)
}

// LoadEnvFile loads an explicitly selected dotenv file and lets it override
// inherited process values. It is intended for administrative commands that
// must validate the exact same file used by deployment scripts.
func LoadEnvFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("environment file path is required")
	}
	if err := godotenv.Overload(path); err != nil {
		return fmt.Errorf("load environment file %s: %w", path, err)
	}
	return nil
}

// LoadDevelopmentEnv loads the nearest repository .env without overriding
// already-defined process variables. Production must always receive explicit
// environment configuration from its runtime.
func LoadDevelopmentEnv() {
	if os.Getenv("APP_ENV") == "production" {
		return
	}
	for _, path := range []string{"../.env", ".env"} {
		if _, err := os.Stat(path); err == nil {
			_ = godotenv.Load(path)
			if _, configured := os.LookupEnv("WENZWORK_ENV_FILE"); !configured {
				if absolutePath, pathErr := filepath.Abs(path); pathErr == nil {
					_ = os.Setenv("WENZWORK_ENV_FILE", absolutePath)
				}
			}
			return
		}
	}
}

func load(lookup lookupFunc) (Config, error) {
	environment := valueOrDefault(lookup, "APP_ENV", "development")
	databaseURLDefault := ""
	redisURLDefault := ""
	if environment == "development" {
		databaseURLDefault = "postgres://wenzwork:wenzwork_dev@localhost:54328/wenzwork?sslmode=disable"
		redisURLDefault = "redis://:wenzwork_redis_dev@localhost:63798/0"
	}
	hostSecretsDirectory := filepath.Dir(valueOrDefault(lookup, "HOST_SECRETS_FILE", defaultHostSecretsFile))
	webRoot := valueOrDefault(lookup, "WEB_ROOT", "")
	if environment == "production" && webRoot == "" {
		webRoot = "web"
	}
	if strings.EqualFold(webRoot, "off") {
		webRoot = ""
	}
	cfg := Config{
		Environment:                        environment,
		EnvironmentFile:                    valueOrDefault(lookup, "WENZWORK_ENV_FILE", ".env"),
		MigrationsDir:                      valueOrDefault(lookup, "MIGRATIONS_DIR", "migrations"),
		PublicBaseURL:                      valueOrDefault(lookup, "PUBLIC_BASE_URL", "http://localhost:5173"),
		HTTPAddr:                           valueOrDefault(lookup, "HTTP_ADDR", ":8080"),
		WebRoot:                            webRoot,
		LogLevel:                           valueOrDefault(lookup, "LOG_LEVEL", "info"),
		DatabaseURL:                        valueOrDefault(lookup, "DATABASE_URL", databaseURLDefault),
		AllowedOrigins:                     splitCSV(valueOrDefault(lookup, "ALLOWED_ORIGINS", "http://localhost:5173")),
		TrustedProxyCIDRs:                  splitCSV(valueOrDefault(lookup, "TRUSTED_PROXY_CIDRS", "127.0.0.1/32,::1/128")),
		GeoIPCityDatabasePath:              valueOrDefault(lookup, "GEOIP_CITY_DATABASE_PATH", ""),
		SMTPHost:                           valueOrDefault(lookup, "SMTP_HOST", "localhost"),
		SMTPUser:                           valueOrDefault(lookup, "SMTP_USER", ""),
		SMTPPassword:                       valueOrDefault(lookup, "SMTP_PASSWORD", ""),
		MailFrom:                           valueOrDefault(lookup, "MAIL_FROM", "WenzWork <noreply@local.wenzwork.test>"),
		S3Endpoint:                         valueOrDefault(lookup, "S3_ENDPOINT", ""),
		S3Region:                           valueOrDefault(lookup, "S3_REGION", ""),
		S3Bucket:                           valueOrDefault(lookup, "S3_BUCKET", ""),
		S3AccessKeyID:                      valueOrDefault(lookup, "S3_ACCESS_KEY_ID", ""),
		S3SecretAccessKey:                  valueOrDefault(lookup, "S3_SECRET_ACCESS_KEY", ""),
		S3SessionToken:                     valueOrDefault(lookup, "S3_SESSION_TOKEN", ""),
		S3AddressingStyle:                  valueOrDefault(lookup, "S3_ADDRESSING_STYLE", "auto"),
		DownloadCDNBaseURL:                 valueOrDefault(lookup, "DOWNLOAD_CDN_BASE_URL", ""),
		ReleaseAssetCacheDir:               valueOrDefault(lookup, "RELEASE_ASSET_CACHE_DIR", "cache/releases"),
		ReleasePushStorageDir:              valueOrDefault(lookup, "RELEASE_PUSH_STORAGE_DIR", "cache/release-push"),
		ReleaseAccessKey:                   valueOrDefault(lookup, "RELEASE_ACCESS_KEY", ""),
		GitHubReleaseRepository:            valueOrDefault(lookup, "GITHUB_RELEASE_REPOSITORY", "lyming99/wenzwork_web"),
		GitHubReleaseToken:                 valueOrDefault(lookup, "GITHUB_RELEASE_TOKEN", ""),
		DesktopGitHubReleaseRepository:     valueOrDefault(lookup, "DESKTOP_GITHUB_RELEASE_REPOSITORY", "lyming99/wenzwork"),
		MobileGitHubReleaseRepository:      valueOrDefault(lookup, "MOBILE_GITHUB_RELEASE_REPOSITORY", "lyming99/wenzwork_mobile"),
		MFAEncryptionKey:                   valueOrDefault(lookup, "MFA_ENCRYPTION_KEY", ""),
		RedemptionCodeHMACKey:              valueOrDefault(lookup, "REDEMPTION_CODE_HMAC_KEY", ""),
		RelayCACertificateFile:             valueOrDefault(lookup, "RELAY_CA_CERTIFICATE_FILE", ""),
		RelayCAPrivateKeyFile:              valueOrDefault(lookup, "RELAY_CA_PRIVATE_KEY_FILE", ""),
		RelayDevelopmentCADir:              valueOrDefault(lookup, "RELAY_DEVELOPMENT_CA_DIR", filepath.Join(hostSecretsDirectory, "relay-ca")),
		RelayDirectoryURL:                  valueOrDefault(lookup, "RELAY_DIRECTORY_URL", "https://localhost:9443"),
		RelayBootstrapAssetsDir:            valueOrDefault(lookup, "RELAY_BOOTSTRAP_ASSETS_DIR", "../deploy/relay"),
		RemoteMVPRegion:                    valueOrDefault(lookup, "REMOTE_MVP_REGION", "cn-dev"),
		RemoteMVPPool:                      valueOrDefault(lookup, "REMOTE_MVP_POOL", "standard"),
		RemoteMVPCell:                      valueOrDefault(lookup, "REMOTE_MVP_CELL", "r017"),
		RedisURL:                           valueOrDefault(lookup, "REDIS_URL", redisURLDefault),
		RelayTicketIssuer:                  valueOrDefault(lookup, "RELAY_TICKET_ISSUER", "wenzwork-control"),
		RelayTicketKeyID:                   valueOrDefault(lookup, "RELAY_TICKET_KEY_ID", "control-connection-v1"),
		RelayTicketPrivateKeyFile:          valueOrDefault(lookup, "RELAY_TICKET_PRIVATE_KEY_FILE", filepath.Join(hostSecretsDirectory, "connection-ticket.key")),
		RelayDeviceLinkGrantKeyID:          valueOrDefault(lookup, "RELAY_DEVICE_LINK_GRANT_KEY_ID", "control-device-link-v1"),
		RelayDeviceLinkGrantPrivateKeyFile: valueOrDefault(lookup, "RELAY_DEVICE_LINK_GRANT_PRIVATE_KEY_FILE", filepath.Join(hostSecretsDirectory, "device-link-grant.key")),
	}

	memory, err := parseUnsignedSetting(lookup, "ARGON2_MEMORY_KIB", 64*1024, 32)
	if err != nil {
		return Config{}, err
	}
	iterations, err := parseUnsignedSetting(lookup, "ARGON2_ITERATIONS", 3, 32)
	if err != nil {
		return Config{}, err
	}
	parallelism, err := parseUnsignedSetting(lookup, "ARGON2_PARALLELISM", 1, 8)
	if err != nil {
		return Config{}, err
	}
	cfg.Argon2MemoryKiB = uint32(memory)
	cfg.Argon2Iterations = uint32(iterations)
	cfg.Argon2Parallelism = uint8(parallelism)
	smtpPort, err := parseUnsignedSetting(lookup, "SMTP_PORT", 1025, 16)
	if err != nil || smtpPort == 0 {
		if err == nil {
			err = errors.New("port must be greater than zero")
		}
		return Config{}, fmt.Errorf("SMTP_PORT is invalid: %w", err)
	}
	cfg.SMTPPort = int(smtpPort)

	secure, err := strconv.ParseBool(valueOrDefault(lookup, "COOKIE_SECURE", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("COOKIE_SECURE must be true or false: %w", err)
	}
	cfg.CookieSecure = secure
	adminMFARequired, err := strconv.ParseBool(valueOrDefault(lookup, "ADMIN_MFA_REQUIRED", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("ADMIN_MFA_REQUIRED must be true or false: %w", err)
	}
	cfg.AdminMFARequired = adminMFARequired
	registrationEnabled, err := strconv.ParseBool(valueOrDefault(lookup, "REGISTRATION_ENABLED", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("REGISTRATION_ENABLED must be true or false: %w", err)
	}
	cfg.RegistrationEnabled = registrationEnabled
	ipGeolocationAPIEnabled, err := strconv.ParseBool(valueOrDefault(lookup, "IP_GEOLOCATION_API_ENABLED", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("IP_GEOLOCATION_API_ENABLED must be true or false: %w", err)
	}
	cfg.IPGeolocationAPIEnabled = ipGeolocationAPIEnabled
	remoteMVPEnabled, err := strconv.ParseBool(valueOrDefault(lookup, "REMOTE_MVP_ENABLED", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("REMOTE_MVP_ENABLED must be true or false: %w", err)
	}
	cfg.RemoteMVPEnabled = remoteMVPEnabled
	setupCompletedValue, setupCompletedPresent := lookup("SYSTEM_SETUP_COMPLETED")
	if !setupCompletedPresent {
		// Existing installations predate the first-login setup flag and must not
		// be sent back through onboarding during an upgrade. New packages always
		// write the flag explicitly as false.
		setupCompletedValue = "true"
	}
	setupCompleted, err := strconv.ParseBool(strings.TrimSpace(setupCompletedValue))
	if err != nil {
		return Config{}, fmt.Errorf("SYSTEM_SETUP_COMPLETED must be true or false: %w", err)
	}
	cfg.SystemSetupCompleted = setupCompleted
	ticketTTL, err := time.ParseDuration(valueOrDefault(lookup, "RELAY_TICKET_TTL", "5m"))
	if err != nil {
		return Config{}, fmt.Errorf("RELAY_TICKET_TTL must be a duration: %w", err)
	}
	cfg.RelayTicketTTL = ticketTTL
	deviceLinkGrantTTL, err := time.ParseDuration(valueOrDefault(lookup, "RELAY_DEVICE_LINK_GRANT_TTL", "0"))
	if err != nil {
		return Config{}, fmt.Errorf("RELAY_DEVICE_LINK_GRANT_TTL must be a duration: %w", err)
	}
	cfg.RelayDeviceLinkGrantTTL = deviceLinkGrantTTL
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var problems []error

	if c.Environment != "development" && c.Environment != "test" && c.Environment != "production" {
		problems = append(problems, fmt.Errorf("APP_ENV must be development, test, or production"))
	}
	if strings.TrimSpace(c.DatabaseURL) == "" {
		problems = append(problems, fmt.Errorf("DATABASE_URL is required"))
	}
	if strings.TrimSpace(c.HTTPAddr) == "" {
		problems = append(problems, fmt.Errorf("HTTP_ADDR is required"))
	}
	for name, path := range map[string]string{
		"WENZWORK_ENV_FILE": c.EnvironmentFile,
		"MIGRATIONS_DIR":    c.MigrationsDir,
	} {
		if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
			problems = append(problems, fmt.Errorf("%s is required and must not contain a null byte", name))
		}
	}
	if strings.ContainsRune(c.WebRoot, '\x00') {
		problems = append(problems, fmt.Errorf("WEB_ROOT must not contain a null byte"))
	}
	if strings.ContainsRune(c.GeoIPCityDatabasePath, '\x00') {
		problems = append(problems, fmt.Errorf("GEOIP_CITY_DATABASE_PATH must not contain a null byte"))
	}
	for _, proxy := range c.TrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(proxy); err != nil {
			problems = append(problems, fmt.Errorf("TRUSTED_PROXY_CIDRS contains invalid CIDR %q", proxy))
		}
	}
	if strings.TrimSpace(c.ReleaseAssetCacheDir) == "" || strings.ContainsRune(c.ReleaseAssetCacheDir, '\x00') {
		problems = append(problems, fmt.Errorf("RELEASE_ASSET_CACHE_DIR is required and must not contain a null byte"))
	}
	if strings.TrimSpace(c.ReleasePushStorageDir) == "" || strings.ContainsRune(c.ReleasePushStorageDir, '\x00') {
		problems = append(problems, fmt.Errorf("RELEASE_PUSH_STORAGE_DIR is required and must not contain a null byte"))
	}
	if c.ReleaseAccessKey != "" && !validReleaseAccessKey(c.ReleaseAccessKey) {
		problems = append(problems, fmt.Errorf("RELEASE_ACCESS_KEY must use the generated release_ format"))
	}
	for name, repository := range map[string]string{
		"GITHUB_RELEASE_REPOSITORY":         c.GitHubReleaseRepository,
		"DESKTOP_GITHUB_RELEASE_REPOSITORY": c.DesktopGitHubReleaseRepository,
		"MOBILE_GITHUB_RELEASE_REPOSITORY":  c.MobileGitHubReleaseRepository,
	} {
		repository = strings.TrimSpace(repository)
		if repository == "" || strings.Count(repository, "/") != 1 || strings.Contains(repository, "..") {
			problems = append(problems, fmt.Errorf("%s must use owner/repository format", name))
		}
	}
	mailConfigured := strings.TrimSpace(c.SMTPHost) != "" || strings.TrimSpace(c.SMTPUser) != "" ||
		c.SMTPPassword != "" || strings.TrimSpace(c.MailFrom) != ""
	if mailConfigured {
		if strings.TrimSpace(c.SMTPHost) == "" {
			problems = append(problems, fmt.Errorf("SMTP_HOST is required when system email is configured"))
		}
		if strings.TrimSpace(c.MailFrom) == "" {
			problems = append(problems, fmt.Errorf("MAIL_FROM is required when system email is configured"))
		}
		if strings.TrimSpace(c.SMTPUser) != "" && c.SMTPPassword == "" {
			problems = append(problems, fmt.Errorf("SMTP_PASSWORD is required when SMTP_USER is configured"))
		}
	}
	if err := validateAbsoluteHTTPURL("PUBLIC_BASE_URL", c.PublicBaseURL); err != nil {
		problems = append(problems, err)
	}
	if len(c.MFAEncryptionKey) < minimumSecretLength || strings.HasPrefix(c.MFAEncryptionKey, "<") {
		problems = append(problems, fmt.Errorf("MFA_ENCRYPTION_KEY must contain at least %d non-placeholder bytes", minimumSecretLength))
	}
	if len(c.RedemptionCodeHMACKey) < minimumSecretLength || strings.HasPrefix(c.RedemptionCodeHMACKey, "<") {
		problems = append(problems, fmt.Errorf("REDEMPTION_CODE_HMAC_KEY must contain at least %d non-placeholder bytes", minimumSecretLength))
	}
	if c.MFAEncryptionKey != "" && c.MFAEncryptionKey == c.RedemptionCodeHMACKey {
		problems = append(problems, fmt.Errorf("MFA_ENCRYPTION_KEY and REDEMPTION_CODE_HMAC_KEY must be independent"))
	}
	if (c.RelayCACertificateFile == "") != (c.RelayCAPrivateKeyFile == "") {
		problems = append(problems, fmt.Errorf("RELAY_CA_CERTIFICATE_FILE and RELAY_CA_PRIVATE_KEY_FILE must be configured together"))
	}
	for name, path := range map[string]string{
		"RELAY_CA_CERTIFICATE_FILE":                c.RelayCACertificateFile,
		"RELAY_CA_PRIVATE_KEY_FILE":                c.RelayCAPrivateKeyFile,
		"RELAY_DEVELOPMENT_CA_DIR":                 c.RelayDevelopmentCADir,
		"RELAY_BOOTSTRAP_ASSETS_DIR":               c.RelayBootstrapAssetsDir,
		"RELAY_TICKET_PRIVATE_KEY_FILE":            c.RelayTicketPrivateKeyFile,
		"RELAY_DEVICE_LINK_GRANT_PRIVATE_KEY_FILE": c.RelayDeviceLinkGrantPrivateKeyFile,
	} {
		if strings.ContainsRune(path, '\x00') || ((name == "RELAY_DEVELOPMENT_CA_DIR" || name == "RELAY_BOOTSTRAP_ASSETS_DIR") && strings.TrimSpace(path) == "") {
			problems = append(problems, fmt.Errorf("%s is invalid", name))
		}
	}
	if strings.TrimSpace(c.RemoteMVPRegion) == "" || len(c.RemoteMVPRegion) > 80 || strings.ContainsAny(c.RemoteMVPRegion, "\r\n\x00") {
		problems = append(problems, fmt.Errorf("REMOTE_MVP_REGION is invalid"))
	}
	if strings.TrimSpace(c.RemoteMVPPool) == "" || len(c.RemoteMVPPool) > 80 || strings.ContainsAny(c.RemoteMVPPool, "\r\n\x00") {
		problems = append(problems, fmt.Errorf("REMOTE_MVP_POOL is invalid"))
	}
	if strings.TrimSpace(c.RemoteMVPCell) == "" || len(c.RemoteMVPCell) > 80 || strings.ContainsAny(c.RemoteMVPCell, "\r\n\x00") {
		problems = append(problems, fmt.Errorf("REMOTE_MVP_CELL is invalid"))
	}
	if c.RelayTicketTTL < time.Minute || c.RelayTicketTTL > 15*time.Minute {
		problems = append(problems, fmt.Errorf("RELAY_TICKET_TTL must be between 1m and 15m"))
	}
	if c.RelayDeviceLinkGrantTTL < 0 || (c.RelayDeviceLinkGrantTTL > 0 && c.RelayDeviceLinkGrantTTL < 5*time.Second) || c.RelayDeviceLinkGrantTTL > 15*time.Minute {
		problems = append(problems, fmt.Errorf("RELAY_DEVICE_LINK_GRANT_TTL must be 0 (persistent) or between 5s and 15m"))
	}
	for name, value := range map[string]string{
		"RELAY_TICKET_ISSUER": c.RelayTicketIssuer, "RELAY_TICKET_KEY_ID": c.RelayTicketKeyID,
		"RELAY_DEVICE_LINK_GRANT_KEY_ID": c.RelayDeviceLinkGrantKeyID,
	} {
		if len(value) > 120 || strings.ContainsAny(value, " \t\r\n\x00") {
			problems = append(problems, fmt.Errorf("%s is invalid", name))
		}
	}
	if err := validateAbsoluteHTTPURL("RELAY_DIRECTORY_URL", c.RelayDirectoryURL); err != nil {
		problems = append(problems, err)
	}
	if c.Argon2MemoryKiB < 19*1024 || c.Argon2MemoryKiB > 256*1024 {
		problems = append(problems, fmt.Errorf("ARGON2_MEMORY_KIB must be between 19456 and 262144"))
	}
	if c.Argon2Iterations < 2 || c.Argon2Iterations > 10 {
		problems = append(problems, fmt.Errorf("ARGON2_ITERATIONS must be between 2 and 10"))
	}
	if c.Argon2Parallelism < 1 || c.Argon2Parallelism > 8 {
		problems = append(problems, fmt.Errorf("ARGON2_PARALLELISM must be between 1 and 8"))
	}
	if c.CookieSecure {
		if parsed, err := url.Parse(c.PublicBaseURL); err == nil && parsed.Scheme != "https" {
			problems = append(problems, fmt.Errorf("PUBLIC_BASE_URL must use https when COOKIE_SECURE is true"))
		}
	}
	if c.Environment == "production" {
		if parsed, err := url.Parse(c.PublicBaseURL); err == nil && parsed.Scheme != "https" {
			problems = append(problems, fmt.Errorf("PUBLIC_BASE_URL must use https in production"))
		}
		if parsed, err := url.Parse(c.RelayDirectoryURL); err == nil && parsed.Scheme != "https" {
			problems = append(problems, fmt.Errorf("RELAY_DIRECTORY_URL must use https in production"))
		}
	}

	return errors.Join(problems...)
}

func (c Config) RemoteMVPConfigurationError() error {
	if !c.RemoteMVPEnabled {
		return errors.New("REMOTE_MVP_ENABLED is false")
	}
	var problems []error
	for name, value := range map[string]string{
		"REDIS_URL": c.RedisURL, "RELAY_TICKET_ISSUER": c.RelayTicketIssuer,
		"RELAY_TICKET_KEY_ID": c.RelayTicketKeyID, "RELAY_TICKET_PRIVATE_KEY_FILE": c.RelayTicketPrivateKeyFile,
		"RELAY_DEVICE_LINK_GRANT_KEY_ID":           c.RelayDeviceLinkGrantKeyID,
		"RELAY_DEVICE_LINK_GRANT_PRIVATE_KEY_FILE": c.RelayDeviceLinkGrantPrivateKeyFile,
	} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Errorf("%s is required", name))
		}
	}
	if c.RelayDeviceLinkGrantKeyID != "" && c.RelayDeviceLinkGrantKeyID == c.RelayTicketKeyID {
		problems = append(problems, fmt.Errorf("RELAY_DEVICE_LINK_GRANT_KEY_ID must differ from RELAY_TICKET_KEY_ID"))
	}
	if c.RelayDeviceLinkGrantPrivateKeyFile != "" && c.RelayDeviceLinkGrantPrivateKeyFile == c.RelayTicketPrivateKeyFile {
		problems = append(problems, fmt.Errorf("RELAY_DEVICE_LINK_GRANT_PRIVATE_KEY_FILE must differ from RELAY_TICKET_PRIVATE_KEY_FILE"))
	}
	return errors.Join(problems...)
}

func valueOrDefault(lookup lookupFunc, key, fallback string) string {
	if value, ok := lookup(key); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func parseUnsignedSetting(lookup lookupFunc, name string, fallback uint64, bitSize int) (uint64, error) {
	raw := valueOrDefault(lookup, name, strconv.FormatUint(fallback, 10))
	value, err := strconv.ParseUint(raw, 10, bitSize)
	if err != nil {
		return 0, fmt.Errorf("%s must be an unsigned integer: %w", name, err)
	}
	return value, nil
}

func validateAbsoluteHTTPURL(name, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
	return nil
}
