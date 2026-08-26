package systemsetup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/config"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
)

var (
	ErrAlreadyComplete        = errors.New("system setup is already complete")
	ErrInvalid                = errors.New("system setup configuration is invalid")
	ErrRedisUnavailable       = errors.New("configured Redis is unavailable")
	ErrDatabaseUnavailable    = errors.New("configured database is unavailable")
	ErrSMTPUnavailable        = errors.New("configured SMTP delivery is unavailable")
	ErrAdministratorBootstrap = errors.New("administrator could not be initialized")
	ErrEnvironmentWrite       = errors.New("system environment could not be saved")
)

type Settings struct {
	Required                bool     `json:"required"`
	PublicBaseURL           string   `json:"publicBaseUrl"`
	DatabaseURL             string   `json:"databaseUrl"`
	RedisURL                string   `json:"redisUrl"`
	SMTPHost                string   `json:"smtpHost"`
	SMTPPort                int      `json:"smtpPort"`
	SMTPUser                string   `json:"smtpUser"`
	SMTPPasswordConfigured  bool     `json:"smtpPasswordConfigured"`
	MailFrom                string   `json:"mailFrom"`
	CookieSecure            bool     `json:"cookieSecure"`
	AdminMFARequired        bool     `json:"adminMfaRequired"`
	RegistrationEnabled     bool     `json:"registrationEnabled"`
	AllowedOrigins          []string `json:"allowedOrigins"`
	WebGitHubRepository     string   `json:"webGithubRepository"`
	DesktopGitHubRepository string   `json:"desktopGithubRepository"`
	MobileGitHubRepository  string   `json:"mobileGithubRepository"`
}

type ApplyInput struct {
	PublicBaseURL           string
	DatabaseURL             string
	RedisURL                string
	SMTPHost                string
	SMTPPort                int
	SMTPUser                string
	SMTPPassword            *string
	ClearSMTPPassword       bool
	MailFrom                string
	CookieSecure            bool
	AdminMFARequired        bool
	RegistrationEnabled     bool
	AllowedOrigins          []string
	WebGitHubRepository     string
	DesktopGitHubRepository string
	MobileGitHubRepository  string
}

type ApplyResult struct {
	Settings        Settings `json:"settings"`
	RestartRequired bool     `json:"restartRequired"`
}

type Service struct {
	applyMutex               sync.Mutex
	current                  config.Config
	administratorEmail       string
	administratorPassword    string
	administratorDisplayName string
	appliedSettings          *Settings
}

func NewService(current config.Config, administratorEmail, administratorPassword, administratorDisplayName string) *Service {
	administratorDisplayName = strings.TrimSpace(administratorDisplayName)
	if administratorDisplayName == "" {
		administratorDisplayName = "WenzWork Administrator"
	}
	return &Service{
		current:                  current,
		administratorEmail:       strings.TrimSpace(administratorEmail),
		administratorPassword:    administratorPassword,
		administratorDisplayName: administratorDisplayName,
	}
}

func (service *Service) Required() bool {
	return service != nil && !service.current.SystemSetupCompleted
}

func (service *Service) Get(context.Context) (Settings, error) {
	if service == nil {
		return Settings{}, ErrInvalid
	}
	service.applyMutex.Lock()
	defer service.applyMutex.Unlock()
	if service.appliedSettings != nil {
		return *service.appliedSettings, nil
	}
	return settingsFromConfig(service.current, service.Required()), nil
}

func (service *Service) Apply(ctx context.Context, input ApplyInput) (ApplyResult, error) {
	if service == nil {
		return ApplyResult{}, ErrInvalid
	}
	service.applyMutex.Lock()
	defer service.applyMutex.Unlock()
	if !service.Required() || service.appliedSettings != nil {
		return ApplyResult{}, ErrAlreadyComplete
	}
	candidate, smtpPassword, err := service.candidate(input)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if service.administratorEmail == "" || len(service.administratorPassword) < 8 {
		return ApplyResult{}, fmt.Errorf("%w: initial administrator email and password are unavailable", ErrAdministratorBootstrap)
	}
	databaseProbe, err := database.Open(ctx, candidate.DatabaseURL)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrDatabaseUnavailable, err)
	}
	probeSQLDatabase, err := databaseProbe.DB()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrDatabaseUnavailable, err)
	}
	if err := probeSQLDatabase.Close(); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: close connection probe: %v", ErrDatabaseUnavailable, err)
	}
	if err := pingRedis(ctx, candidate.RedisURL); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrRedisUnavailable, err)
	}
	sender, err := mailer.NewSMTPSender(mailer.SMTPConfig{
		Host: candidate.SMTPHost, Port: candidate.SMTPPort, Username: candidate.SMTPUser,
		Password: smtpPassword, From: candidate.MailFrom,
		RequireTLS: candidate.Environment == "production", Timeout: 10 * time.Second,
	})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: SMTP: %v", ErrInvalid, err)
	}
	if err := sendAdministratorTestEmail(ctx, sender, service.administratorEmail); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrSMTPUnavailable, err)
	}
	if err := database.Migrate(ctx, candidate.DatabaseURL, candidate.MigrationsDir); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrDatabaseUnavailable, err)
	}
	targetDatabase, err := database.Open(ctx, candidate.DatabaseURL)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrDatabaseUnavailable, err)
	}
	sqlDatabase, err := targetDatabase.DB()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrDatabaseUnavailable, err)
	}
	defer sqlDatabase.Close()
	_, err = auth.BootstrapSuperAdmin(ctx, targetDatabase, service.administratorEmail, service.administratorPassword,
		service.administratorDisplayName, auth.Argon2Params{
			MemoryKiB: candidate.Argon2MemoryKiB, Iterations: candidate.Argon2Iterations,
			Parallelism: candidate.Argon2Parallelism, SaltLength: 16, KeyLength: 32,
		})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrAdministratorBootstrap, err)
	}

	updates := map[string]string{
		"APP_ENV":                           candidate.Environment,
		"PUBLIC_BASE_URL":                   candidate.PublicBaseURL,
		"DATABASE_URL":                      candidate.DatabaseURL,
		"REDIS_URL":                         candidate.RedisURL,
		"SMTP_HOST":                         candidate.SMTPHost,
		"SMTP_PORT":                         strconv.Itoa(candidate.SMTPPort),
		"SMTP_USER":                         candidate.SMTPUser,
		"SMTP_PASSWORD":                     smtpPassword,
		"MAIL_FROM":                         candidate.MailFrom,
		"COOKIE_SECURE":                     strconv.FormatBool(candidate.CookieSecure),
		"ADMIN_MFA_REQUIRED":                strconv.FormatBool(candidate.AdminMFARequired),
		"REGISTRATION_ENABLED":              strconv.FormatBool(candidate.RegistrationEnabled),
		"ALLOWED_ORIGINS":                   strings.Join(candidate.AllowedOrigins, ","),
		"GITHUB_RELEASE_REPOSITORY":         candidate.GitHubReleaseRepository,
		"DESKTOP_GITHUB_RELEASE_REPOSITORY": candidate.DesktopGitHubReleaseRepository,
		"MOBILE_GITHUB_RELEASE_REPOSITORY":  candidate.MobileGitHubReleaseRepository,
		"SYSTEM_ADMIN_EMAIL":                service.administratorEmail,
		"SYSTEM_ADMIN_PASSWORD":             "",
		"SYSTEM_ADMIN_DISPLAY_NAME":         service.administratorDisplayName,
		"SYSTEM_SETUP_COMPLETED":            "true",
	}
	if err := config.UpdateEnvFile(candidate.EnvironmentFile, updates); err != nil {
		return ApplyResult{}, fmt.Errorf("%w: %v", ErrEnvironmentWrite, err)
	}
	completed := settingsFromConfig(candidate, false)
	completed.SMTPPasswordConfigured = smtpPassword != ""
	service.appliedSettings = &completed
	service.administratorPassword = ""
	_ = os.Unsetenv("SYSTEM_ADMIN_PASSWORD")
	return ApplyResult{Settings: completed, RestartRequired: true}, nil
}

func sendAdministratorTestEmail(ctx context.Context, sender mailer.Sender, administratorEmail string) error {
	if sender == nil {
		return errors.New("SMTP sender is unavailable")
	}
	return sender.Send(ctx, mailer.Message{
		To:      strings.TrimSpace(administratorEmail),
		Subject: "WenzWork system setup email test",
		Text: "WenzWork successfully delivered this administrator test email. " +
			"The first-deployment setup can continue after the remaining checks pass.\n",
	})
}

func (service *Service) candidate(input ApplyInput) (config.Config, string, error) {
	candidate := service.current
	candidate.PublicBaseURL = strings.TrimSpace(input.PublicBaseURL)
	candidate.DatabaseURL = strings.TrimSpace(input.DatabaseURL)
	candidate.RedisURL = strings.TrimSpace(input.RedisURL)
	candidate.SMTPHost = strings.TrimSpace(input.SMTPHost)
	candidate.SMTPPort = input.SMTPPort
	candidate.SMTPUser = strings.TrimSpace(input.SMTPUser)
	candidate.MailFrom = strings.TrimSpace(input.MailFrom)
	candidate.CookieSecure = input.CookieSecure
	candidate.AdminMFARequired = input.AdminMFARequired
	candidate.RegistrationEnabled = input.RegistrationEnabled
	candidate.AllowedOrigins = normalizeOrigins(input.AllowedOrigins)
	candidate.GitHubReleaseRepository = strings.TrimSpace(input.WebGitHubRepository)
	candidate.DesktopGitHubReleaseRepository = strings.TrimSpace(input.DesktopGitHubRepository)
	candidate.MobileGitHubReleaseRepository = strings.TrimSpace(input.MobileGitHubRepository)
	candidate.SystemSetupCompleted = true

	parsed, err := url.Parse(candidate.PublicBaseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return config.Config{}, "", errors.New("public site URL is invalid")
	}
	if parsed.Scheme == "https" {
		candidate.Environment = "production"
	} else if parsed.Scheme == "http" && loopbackHost(parsed.Hostname()) {
		candidate.Environment = "development"
	} else {
		return config.Config{}, "", errors.New("public site URL must use HTTPS unless it is an exact loopback host")
	}
	if len(candidate.AllowedOrigins) == 0 {
		candidate.AllowedOrigins = []string{strings.TrimRight(candidate.PublicBaseURL, "/")}
	}
	smtpPassword := service.current.SMTPPassword
	if input.ClearSMTPPassword {
		smtpPassword = ""
	} else if input.SMTPPassword != nil {
		smtpPassword = *input.SMTPPassword
	}
	candidate.SMTPPassword = smtpPassword
	if candidate.SMTPUser != "" && smtpPassword == "" {
		return config.Config{}, "", errors.New("SMTP password is required when an SMTP username is configured")
	}
	for name, value := range map[string]string{
		"PUBLIC_BASE_URL": candidate.PublicBaseURL, "DATABASE_URL": candidate.DatabaseURL,
		"REDIS_URL": candidate.RedisURL, "SMTP_HOST": candidate.SMTPHost,
		"SMTP_USER": candidate.SMTPUser, "SMTP_PASSWORD": smtpPassword,
		"MAIL_FROM": candidate.MailFrom,
	} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return config.Config{}, "", fmt.Errorf("%s contains a forbidden character", name)
		}
	}
	for _, origin := range candidate.AllowedOrigins {
		parsedOrigin, originErr := url.Parse(origin)
		if originErr != nil || (parsedOrigin.Scheme != "http" && parsedOrigin.Scheme != "https") ||
			parsedOrigin.Host == "" || parsedOrigin.User != nil || (parsedOrigin.Path != "" && parsedOrigin.Path != "/") ||
			parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" {
			return config.Config{}, "", fmt.Errorf("allowed origin %q is invalid", origin)
		}
	}
	if err := candidate.Validate(); err != nil {
		return config.Config{}, "", err
	}
	return candidate, smtpPassword, nil
}

func settingsFromConfig(current config.Config, required bool) Settings {
	return Settings{
		Required: required, PublicBaseURL: current.PublicBaseURL,
		DatabaseURL: current.DatabaseURL, RedisURL: current.RedisURL,
		SMTPHost: current.SMTPHost, SMTPPort: current.SMTPPort, SMTPUser: current.SMTPUser,
		SMTPPasswordConfigured: current.SMTPPassword != "", MailFrom: current.MailFrom,
		CookieSecure:            current.CookieSecure,
		AdminMFARequired:        current.AdminMFARequired,
		RegistrationEnabled:     current.RegistrationEnabled,
		AllowedOrigins:          append([]string(nil), current.AllowedOrigins...),
		WebGitHubRepository:     current.GitHubReleaseRepository,
		DesktopGitHubRepository: current.DesktopGitHubReleaseRepository,
		MobileGitHubRepository:  current.MobileGitHubReleaseRepository,
	}
}

func normalizeOrigins(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimRight(strings.TrimSpace(value), "/")
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func pingRedis(ctx context.Context, rawURL string) error {
	options, err := redis.ParseURL(strings.TrimSpace(rawURL))
	if err != nil {
		return err
	}
	client := redis.NewClient(options)
	defer client.Close()
	pingContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return client.Ping(pingContext).Err()
}
