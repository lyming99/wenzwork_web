package systemsetup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wenzwork/wenzwork-web/server/internal/config"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
)

type recordingSender struct {
	message mailer.Message
	err     error
}

func (sender *recordingSender) Send(_ context.Context, message mailer.Message) error {
	sender.message = message
	return sender.err
}

func TestCandidatePromotesHTTPSConfigurationToProduction(t *testing.T) {
	service := NewService(validCurrentConfig(t), "admin@example.test", "administrator-password", "Administrator")
	password := `smtp \\ password with " quote`
	candidate, smtpPassword, err := service.candidate(ApplyInput{
		PublicBaseURL:           "https://control.example.test/",
		DatabaseURL:             "postgres://wenzwork:secret@db.example.test/wenzwork?sslmode=require",
		RedisURL:                "redis://:secret@redis.example.test:6379/0",
		SMTPHost:                "smtp.example.test",
		SMTPPort:                587,
		SMTPUser:                "mailer@example.test",
		SMTPPassword:            &password,
		MailFrom:                "WenzWork <noreply@example.test>",
		CookieSecure:            false,
		AdminMFARequired:        false,
		RegistrationEnabled:     false,
		AllowedOrigins:          []string{"https://control.example.test/", " https://control.example.test "},
		WebGitHubRepository:     "acme/wenzwork-web",
		DesktopGitHubRepository: "acme/wenzwork-desktop",
		MobileGitHubRepository:  "acme/wenzwork-mobile",
	})
	if err != nil {
		t.Fatalf("candidate() error = %v", err)
	}
	if candidate.Environment != "production" || candidate.CookieSecure || candidate.AdminMFARequired || !candidate.SystemSetupCompleted {
		t.Fatalf("production candidate = environment %q secure %v MFA %v complete %v", candidate.Environment, candidate.CookieSecure, candidate.AdminMFARequired, candidate.SystemSetupCompleted)
	}
	if len(candidate.AllowedOrigins) != 1 || candidate.AllowedOrigins[0] != "https://control.example.test" {
		t.Fatalf("normalized origins = %#v", candidate.AllowedOrigins)
	}
	if smtpPassword != password || candidate.SMTPPassword != password {
		t.Fatal("SMTP password was not carried into the candidate")
	}
	settings := settingsFromConfig(candidate, false)
	if !settings.SMTPPasswordConfigured || settings.Required {
		t.Fatalf("public settings leaked incorrect setup state: %+v", settings)
	}
}

func TestCandidateKeepsProductionSecurityOptionsExplicit(t *testing.T) {
	service := NewService(validCurrentConfig(t), "admin@example.test", "administrator-password", "Administrator")
	candidate, _, err := service.candidate(ApplyInput{
		PublicBaseURL:           "https://control.example.test",
		DatabaseURL:             "postgres://wenzwork:secret@db.example.test/wenzwork?sslmode=require",
		RedisURL:                "redis://:secret@redis.example.test:6379/0",
		SMTPHost:                "smtp.example.test",
		SMTPPort:                587,
		MailFrom:                "WenzWork <noreply@example.test>",
		CookieSecure:            true,
		AdminMFARequired:        true,
		WebGitHubRepository:     "acme/wenzwork-web",
		DesktopGitHubRepository: "acme/wenzwork-desktop",
		MobileGitHubRepository:  "acme/wenzwork-mobile",
	})
	if err != nil {
		t.Fatalf("candidate() error = %v", err)
	}
	if candidate.Environment != "production" || !candidate.CookieSecure || !candidate.AdminMFARequired {
		t.Fatalf("explicit production security options = environment %q secure %v MFA %v", candidate.Environment, candidate.CookieSecure, candidate.AdminMFARequired)
	}
	settings := settingsFromConfig(candidate, false)
	if !settings.CookieSecure || !settings.AdminMFARequired {
		t.Fatalf("public settings lost explicit security options: %+v", settings)
	}
}

func TestCandidateRejectsNonLoopbackPlainHTTP(t *testing.T) {
	service := NewService(validCurrentConfig(t), "admin@example.test", "administrator-password", "Administrator")
	_, _, err := service.candidate(ApplyInput{
		PublicBaseURL:           "http://control.example.test",
		DatabaseURL:             "postgres://wenzwork@example.test/wenzwork",
		RedisURL:                "redis://redis.example.test:6379/0",
		SMTPHost:                "smtp.example.test",
		SMTPPort:                587,
		MailFrom:                "WenzWork <noreply@example.test>",
		WebGitHubRepository:     "acme/wenzwork-web",
		DesktopGitHubRepository: "acme/wenzwork-desktop",
		MobileGitHubRepository:  "acme/wenzwork-mobile",
	})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("candidate() error = %v, want HTTPS validation", err)
	}
}

func TestSendAdministratorTestEmailTargetsBootstrapAdministrator(t *testing.T) {
	sender := &recordingSender{}
	if err := sendAdministratorTestEmail(context.Background(), sender, " admin@example.test "); err != nil {
		t.Fatalf("sendAdministratorTestEmail() error = %v", err)
	}
	if sender.message.To != "admin@example.test" || sender.message.Subject == "" || sender.message.Text == "" {
		t.Fatalf("administrator test message = %+v", sender.message)
	}
}

func TestSendAdministratorTestEmailReturnsDeliveryFailure(t *testing.T) {
	want := errors.New("SMTP rejected recipient")
	err := sendAdministratorTestEmail(context.Background(), &recordingSender{err: want}, "admin@example.test")
	if !errors.Is(err, want) {
		t.Fatalf("sendAdministratorTestEmail() error = %v, want %v", err, want)
	}
}

func validCurrentConfig(t *testing.T) config.Config {
	t.Helper()
	secretDirectory := t.TempDir()
	return config.Config{
		Environment: "development", EnvironmentFile: filepath.Join(secretDirectory, ".env"),
		MigrationsDir: "migrations", PublicBaseURL: "http://localhost:8080", HTTPAddr: ":8080",
		DatabaseURL: "postgres://wenzwork@localhost/wenzwork", RedisURL: "redis://localhost:6379/0",
		AllowedOrigins: []string{"http://localhost:8080"}, RegistrationEnabled: true,
		SMTPHost: "localhost", SMTPPort: 1025, MailFrom: "WenzWork <noreply@local.wenzwork.test>",
		ReleaseAssetCacheDir: "cache/releases", ReleasePushStorageDir: filepath.Join(secretDirectory, "release-push"),
		GitHubReleaseRepository:        "acme/wenzwork-web",
		DesktopGitHubReleaseRepository: "acme/wenzwork-desktop", MobileGitHubReleaseRepository: "acme/wenzwork-mobile",
		MFAEncryptionKey: strings.Repeat("m", 32), RedemptionCodeHMACKey: strings.Repeat("r", 32),
		RelayDevelopmentCADir: filepath.Join(secretDirectory, "relay-ca"), RelayDirectoryURL: "https://localhost:9443",
		RelayBootstrapAssetsDir: "relay-bootstrap", RemoteMVPEnabled: true, RemoteMVPRegion: "cn-dev",
		RemoteMVPPool: "standard", RemoteMVPCell: "r017", RelayTicketIssuer: "wenzwork-control",
		RelayTicketKeyID: "connection-v1", RelayTicketPrivateKeyFile: filepath.Join(secretDirectory, "connection.key"),
		RelayTicketTTL: 5 * time.Minute, RelayDeviceLinkGrantKeyID: "device-link-v1",
		RelayDeviceLinkGrantPrivateKeyFile: filepath.Join(secretDirectory, "device-link.key"), RelayDeviceLinkGrantTTL: 90 * time.Second,
		Argon2MemoryKiB: 64 * 1024, Argon2Iterations: 3, Argon2Parallelism: 1,
	}
}
