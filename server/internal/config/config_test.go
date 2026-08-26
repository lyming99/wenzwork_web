package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadEnvFileOverloadsInheritedValues(t *testing.T) {
	t.Setenv("SMTP_HOST", "inherited.example.test")
	t.Setenv("SMTP_PASSWORD", "inherited-password")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("SMTP_HOST=file.example.test\nSMTP_PASSWORD=\"quoted-value\"\n"), 0o600); err != nil {
		t.Fatalf("write env fixture: %v", err)
	}
	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile() error = %v", err)
	}
	if got := os.Getenv("SMTP_HOST"); got != "file.example.test" {
		t.Fatalf("SMTP_HOST = %q, want file.example.test", got)
	}
	if got := os.Getenv("SMTP_PASSWORD"); got != "quoted-value" {
		t.Fatalf("SMTP_PASSWORD = %q, want quoted-value", got)
	}
}

func TestLoadDevelopmentEnvTracksSelectedEnvironmentFile(t *testing.T) {
	repositoryRoot := t.TempDir()
	serverDirectory := filepath.Join(repositoryRoot, "server")
	if err := os.Mkdir(serverDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	environmentPath := filepath.Join(repositoryRoot, ".env")
	if err := os.WriteFile(environmentPath, []byte("WENZWORK_DEVELOPMENT_ENV_TEST=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(serverDirectory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDirectory) })

	restoreEnvironment := func(name string) {
		value, present := os.LookupEnv(name)
		_ = os.Unsetenv(name)
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	restoreEnvironment("APP_ENV")
	restoreEnvironment("WENZWORK_ENV_FILE")
	restoreEnvironment("WENZWORK_DEVELOPMENT_ENV_TEST")

	LoadDevelopmentEnv()

	if got := os.Getenv("WENZWORK_ENV_FILE"); got != environmentPath {
		t.Fatalf("WENZWORK_ENV_FILE = %q, want %q", got, environmentPath)
	}
	if got := os.Getenv("WENZWORK_DEVELOPMENT_ENV_TEST"); got != "loaded" {
		t.Fatalf("development environment value = %q, want loaded", got)
	}
}

func TestLoadAcceptsValidDevelopmentConfiguration(t *testing.T) {
	values := validValues()
	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Fatalf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
	if cfg.S3Bucket != "wenzwork-releases" || cfg.DownloadCDNBaseURL != "https://downloads.example.test" {
		t.Fatalf("release storage config = bucket %q CDN %q", cfg.S3Bucket, cfg.DownloadCDNBaseURL)
	}
	if cfg.ReleaseAssetCacheDir != "cache/releases" || cfg.GitHubReleaseRepository != "acme/wenzwork" || cfg.GitHubReleaseToken != "github-token" {
		t.Fatalf("release delivery config = cache %q repository %q", cfg.ReleaseAssetCacheDir, cfg.GitHubReleaseRepository)
	}
	if cfg.WebRoot != "" {
		t.Fatalf("WebRoot = %q, want disabled in development", cfg.WebRoot)
	}
	if len(cfg.AllowedOrigins) != 2 {
		t.Fatalf("AllowedOrigins length = %d, want 2", len(cfg.AllowedOrigins))
	}
	if len(cfg.TrustedProxyCIDRs) != 2 || cfg.GeoIPCityDatabasePath != "geo/GeoLite2-City.mmdb" {
		t.Fatalf("analytics network config = proxies %v GeoIP %q", cfg.TrustedProxyCIDRs, cfg.GeoIPCityDatabasePath)
	}
	if !cfg.IPGeolocationAPIEnabled {
		t.Fatal("IPGeolocationAPIEnabled = false, want true by default")
	}
	if cfg.Argon2MemoryKiB != 64*1024 || cfg.Argon2Iterations != 3 || cfg.Argon2Parallelism != 1 {
		t.Fatalf("Argon2 defaults = %d/%d/%d", cfg.Argon2MemoryKiB, cfg.Argon2Iterations, cfg.Argon2Parallelism)
	}
	if !cfg.RegistrationEnabled || cfg.SMTPPort != 1025 || !cfg.RemoteMVPEnabled {
		t.Fatalf("runtime defaults: registration=%v smtpPort=%d remote=%v", cfg.RegistrationEnabled, cfg.SMTPPort, cfg.RemoteMVPEnabled)
	}
	if cfg.AdminMFARequired {
		t.Fatal("AdminMFARequired = true, want explicit opt-in default")
	}
}

func TestLoadAcceptsCoreOnlyHostConfiguration(t *testing.T) {
	values := map[string]string{
		"DATABASE_URL":             "postgres://wenzwork@example.test/wenzwork",
		"REDIS_URL":                "redis://redis.example.test:6379/0",
		"SMTP_HOST":                "smtp.example.test",
		"SMTP_PORT":                "587",
		"SMTP_USER":                "mailer@example.test",
		"SMTP_PASSWORD":            "smtp-password",
		"MAIL_FROM":                "WenzWork <noreply@example.test>",
		"MFA_ENCRYPTION_KEY":       strings.Repeat("m", 32),
		"REDEMPTION_CODE_HMAC_KEY": strings.Repeat("r", 32),
	}
	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load() core-only configuration error = %v", err)
	}
	if cfg.Environment != "development" || cfg.PublicBaseURL != "http://localhost:5173" || cfg.HTTPAddr != ":8080" ||
		!cfg.RegistrationEnabled || !cfg.RemoteMVPEnabled || cfg.RelayTicketIssuer != "wenzwork-control" ||
		cfg.RelayTicketKeyID == cfg.RelayDeviceLinkGrantKeyID || cfg.RelayTicketPrivateKeyFile == cfg.RelayDeviceLinkGrantPrivateKeyFile {
		t.Fatalf("core-only configuration did not receive complete defaults: %+v", cfg)
	}
	if remoteErr := cfg.RemoteMVPConfigurationError(); remoteErr != nil {
		t.Fatalf("RemoteMVPConfigurationError() = %v for core-only configuration", remoteErr)
	}
}

func TestLoadTreatsOnlyExplicitFalseSetupFlagAsNewInstallation(t *testing.T) {
	values := validValues()
	delete(values, "SYSTEM_SETUP_COMPLETED")
	legacy, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.SystemSetupCompleted {
		t.Fatal("legacy configuration without setup flag was sent through onboarding")
	}

	values["SYSTEM_SETUP_COMPLETED"] = "false"
	fresh, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SystemSetupCompleted {
		t.Fatal("explicit new-installation setup flag was ignored")
	}
}

func TestGeneratedHostSecretsAreStableAndPrivate(t *testing.T) {
	values := validValues()
	delete(values, "MFA_ENCRYPTION_KEY")
	delete(values, "REDEMPTION_CODE_HMAC_KEY")
	secretPath := filepath.Join(t.TempDir(), "host-secrets", "application.env")
	values["HOST_SECRETS_FILE"] = secretPath
	baseLookup := func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}

	firstLookup, err := withGeneratedHostSecrets(baseLookup)
	if err != nil {
		t.Fatalf("withGeneratedHostSecrets() error = %v", err)
	}
	first, err := load(firstLookup)
	if err != nil {
		t.Fatalf("load() with generated secrets error = %v", err)
	}
	secondLookup, err := withGeneratedHostSecrets(baseLookup)
	if err != nil {
		t.Fatalf("second withGeneratedHostSecrets() error = %v", err)
	}
	second, err := load(secondLookup)
	if err != nil {
		t.Fatalf("second load() with generated secrets error = %v", err)
	}
	if first.MFAEncryptionKey != second.MFAEncryptionKey || first.RedemptionCodeHMACKey != second.RedemptionCodeHMACKey ||
		first.MFAEncryptionKey == first.RedemptionCodeHMACKey {
		t.Fatal("generated Host secrets were not stable and independent")
	}
	if first.RelayTicketPrivateKeyFile != filepath.Join(filepath.Dir(secretPath), "connection-ticket.key") ||
		first.RelayDeviceLinkGrantPrivateKeyFile != filepath.Join(filepath.Dir(secretPath), "device-link-grant.key") ||
		first.RelayDevelopmentCADir != filepath.Join(filepath.Dir(secretPath), "relay-ca") {
		t.Fatalf("Host secret defaults do not share the protected directory: %+v", first)
	}
	if info, statErr := os.Stat(secretPath); statErr != nil {
		t.Fatalf("stat generated Host secrets: %v", statErr)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("generated Host secrets mode = %o, want 600", info.Mode().Perm())
	}
}

func TestGeneratedReleaseAccessKeyIsStableAndPrivate(t *testing.T) {
	values := validValues()
	delete(values, "RELEASE_ACCESS_KEY")
	keyPath := filepath.Join(t.TempDir(), "host-secrets", "release-access-key")
	values["RELEASE_ACCESS_KEY_FILE"] = keyPath
	lookup := func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}

	firstLookup, err := withGeneratedReleaseAccessKey(lookup)
	if err != nil {
		t.Fatalf("withGeneratedReleaseAccessKey() error = %v", err)
	}
	secondLookup, err := withGeneratedReleaseAccessKey(lookup)
	if err != nil {
		t.Fatalf("second withGeneratedReleaseAccessKey() error = %v", err)
	}
	first, _ := firstLookup("RELEASE_ACCESS_KEY")
	second, _ := secondLookup("RELEASE_ACCESS_KEY")
	if first != second || !validReleaseAccessKey(first) {
		t.Fatalf("generated Release Access Key was not stable or valid: %q / %q", first, second)
	}
	if info, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("stat generated Release Access Key: %v", statErr)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("generated Release Access Key mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadDefaultsProductionWebRoot(t *testing.T) {
	values := validValues()
	values["APP_ENV"] = "production"
	values["PUBLIC_BASE_URL"] = "https://wenzwork.example"
	values["COOKIE_SECURE"] = "true"

	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if cfg.WebRoot != "web" {
		t.Fatalf("WebRoot = %q, want web", cfg.WebRoot)
	}
	if cfg.RelayCACertificateFile != "" || cfg.RelayCAPrivateKeyFile != "" || cfg.RelayDevelopmentCADir == "" {
		t.Fatalf("production Relay CA defaults = certificate %q key %q generated directory %q", cfg.RelayCACertificateFile, cfg.RelayCAPrivateKeyFile, cfg.RelayDevelopmentCADir)
	}
}

func TestLoadAllowsProductionWebServingToBeDisabled(t *testing.T) {
	values := validValues()
	values["APP_ENV"] = "production"
	values["PUBLIC_BASE_URL"] = "https://wenzwork.example"
	values["COOKIE_SECURE"] = "true"
	values["WEB_ROOT"] = "off"

	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if cfg.WebRoot != "" {
		t.Fatalf("WebRoot = %q, want disabled", cfg.WebRoot)
	}
}

func TestLoadAllowsProductionWithoutSecureCookie(t *testing.T) {
	values := validValues()
	values["APP_ENV"] = "production"
	values["PUBLIC_BASE_URL"] = "https://wenzwork.example"
	values["COOKIE_SECURE"] = "false"

	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if cfg.CookieSecure || cfg.AdminMFARequired {
		t.Fatalf("production opt-in security flags = cookie %v MFA %v, want both false", cfg.CookieSecure, cfg.AdminMFARequired)
	}
}

func TestLoadAllowsExplicitProductionSecurityOptIns(t *testing.T) {
	values := validValues()
	values["APP_ENV"] = "production"
	values["PUBLIC_BASE_URL"] = "https://wenzwork.example"
	values["COOKIE_SECURE"] = "true"
	values["ADMIN_MFA_REQUIRED"] = "true"

	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if !cfg.CookieSecure || !cfg.AdminMFARequired {
		t.Fatalf("production opt-in security flags = cookie %v MFA %v, want both true", cfg.CookieSecure, cfg.AdminMFARequired)
	}
}

func TestLoadRejectsSecureCookieForPlainHTTP(t *testing.T) {
	values := validValues()
	values["COOKIE_SECURE"] = "true"

	_, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err == nil || !strings.Contains(err.Error(), "COOKIE_SECURE") || !strings.Contains(err.Error(), "https") {
		t.Fatalf("load() error = %v, want secure-cookie HTTPS validation", err)
	}
}

func TestLoadRejectsInvalidAdminMFAFlag(t *testing.T) {
	values := validValues()
	values["ADMIN_MFA_REQUIRED"] = "sometimes"

	_, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err == nil || !strings.Contains(err.Error(), "ADMIN_MFA_REQUIRED") {
		t.Fatalf("load() error = %v, want administrator MFA validation error", err)
	}
}

func TestLoadRejectsSharedOrPlaceholderSecrets(t *testing.T) {
	values := validValues()
	values["MFA_ENCRYPTION_KEY"] = "<placeholder>"
	values["REDEMPTION_CODE_HMAC_KEY"] = "<placeholder>"

	_, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err == nil {
		t.Fatal("load() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "MFA_ENCRYPTION_KEY") || !strings.Contains(err.Error(), "REDEMPTION_CODE_HMAC_KEY") {
		t.Fatalf("load() error = %q, want both secret problems", err)
	}
}

func TestLoadRejectsWeakOrExcessiveArgon2Configuration(t *testing.T) {
	values := validValues()
	values["ARGON2_MEMORY_KIB"] = "1024"
	values["ARGON2_ITERATIONS"] = "1000"
	values["ARGON2_PARALLELISM"] = "0"

	_, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err == nil {
		t.Fatal("load() error = nil, want Argon2 validation errors")
	}
	for _, name := range []string{"ARGON2_MEMORY_KIB", "ARGON2_ITERATIONS", "ARGON2_PARALLELISM"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("load() error = %q, want %s", err, name)
		}
	}
}

func TestLoadRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	values := validValues()
	values["TRUSTED_PROXY_CIDRS"] = "127.0.0.1/32,not-a-network"

	_, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("load() error = %v, want trusted proxy validation error", err)
	}
}

func TestLoadAllowsExternalIPGeolocationToBeDisabled(t *testing.T) {
	values := validValues()
	values["IP_GEOLOCATION_API_ENABLED"] = "false"

	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if cfg.IPGeolocationAPIEnabled {
		t.Fatal("IPGeolocationAPIEnabled = true, want false")
	}
}

func TestLoadRejectsInvalidExternalIPGeolocationFlag(t *testing.T) {
	values := validValues()
	values["IP_GEOLOCATION_API_ENABLED"] = "sometimes"

	_, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err == nil || !strings.Contains(err.Error(), "IP_GEOLOCATION_API_ENABLED") {
		t.Fatalf("load() error = %v, want external IP geolocation validation error", err)
	}
}

func TestRemoteMVPConfigurationDoesNotRequireNATS(t *testing.T) {
	values := validValues()
	values["REDIS_URL"] = ""
	cfg, err := load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if remoteErr := cfg.RemoteMVPConfigurationError(); remoteErr == nil || !strings.Contains(remoteErr.Error(), "REDIS_URL") ||
		strings.Contains(remoteErr.Error(), "NATS_URL") || strings.Contains(remoteErr.Error(), "RELAY_TICKET_PRIVATE_KEY_FILE") ||
		strings.Contains(remoteErr.Error(), "RELAY_DEVICE_LINK_GRANT_PRIVATE_KEY_FILE") {
		t.Fatalf("RemoteMVPConfigurationError() = %v", remoteErr)
	}

	values["REDIS_URL"] = "redis://localhost:6379/0"
	cfg, err = load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if remoteErr := cfg.RemoteMVPConfigurationError(); remoteErr != nil {
		t.Fatalf("RemoteMVPConfigurationError() = %v", remoteErr)
	}
	if cfg.RemoteMVPRegion != "cn-dev" || cfg.RemoteMVPPool != "standard" || cfg.RemoteMVPCell != "r017" || cfg.RelayTicketTTL != 5*time.Minute ||
		cfg.RelayDeviceLinkGrantTTL != 0 || cfg.RelayTicketIssuer != "wenzwork-control" ||
		cfg.RelayTicketKeyID == cfg.RelayDeviceLinkGrantKeyID || cfg.RelayTicketPrivateKeyFile == cfg.RelayDeviceLinkGrantPrivateKeyFile {
		t.Fatalf("remote/v2 defaults = region=%q pool=%q cell=%q ttl=%s device_link_ttl=%s", cfg.RemoteMVPRegion, cfg.RemoteMVPPool, cfg.RemoteMVPCell, cfg.RelayTicketTTL, cfg.RelayDeviceLinkGrantTTL)
	}
}

func TestLoadRejectsInvalidRemoteMVPSettings(t *testing.T) {
	for name, setting := range map[string]struct{ key, value string }{
		"enabled":   {"REMOTE_MVP_ENABLED", "sometimes"},
		"ttl":       {"RELAY_TICKET_TTL", "30m"},
		"grant ttl": {"RELAY_DEVICE_LINK_GRANT_TTL", "4s"},
		"region":    {"REMOTE_MVP_REGION", "bad\nregion"},
		"cell":      {"REMOTE_MVP_CELL", "bad\ncell"},
		"key id":    {"RELAY_TICKET_KEY_ID", "contains spaces"},
	} {
		t.Run(name, func(t *testing.T) {
			values := validValues()
			values[setting.key] = setting.value
			_, err := load(func(key string) (string, bool) {
				value, ok := values[key]
				return value, ok
			})
			if err == nil {
				t.Fatalf("load() accepted %s=%q", setting.key, setting.value)
			}
		})
	}
}

func validValues() map[string]string {
	return map[string]string{
		"APP_ENV":                   "development",
		"PUBLIC_BASE_URL":           "http://localhost:5173",
		"HTTP_ADDR":                 ":9090",
		"DATABASE_URL":              "postgres://example",
		"REDIS_URL":                 "redis://localhost:6379/0",
		"COOKIE_SECURE":             "false",
		"ALLOWED_ORIGINS":           "http://localhost:5173, http://127.0.0.1:5173",
		"TRUSTED_PROXY_CIDRS":       "127.0.0.1/32,::1/128",
		"GEOIP_CITY_DATABASE_PATH":  "geo/GeoLite2-City.mmdb",
		"S3_ENDPOINT":               "https://s3.example.test",
		"S3_REGION":                 "test-1",
		"S3_BUCKET":                 "wenzwork-releases",
		"S3_ACCESS_KEY_ID":          "test-access-key",
		"S3_SECRET_ACCESS_KEY":      "test-secret-key",
		"S3_ADDRESSING_STYLE":       "path",
		"DOWNLOAD_CDN_BASE_URL":     "https://downloads.example.test",
		"RELEASE_ASSET_CACHE_DIR":   "cache/releases",
		"GITHUB_RELEASE_REPOSITORY": "acme/wenzwork",
		"GITHUB_RELEASE_TOKEN":      "github-token",
		"MFA_ENCRYPTION_KEY":        strings.Repeat("m", 32),
		"REDEMPTION_CODE_HMAC_KEY":  strings.Repeat("r", 32),
	}
}
