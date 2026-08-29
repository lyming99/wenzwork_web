// Package emailsettings owns the database-backed system email configuration.
// Database settings take precedence; the Host environment remains the fallback.
package emailsettings

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInvalid       = errors.New("system email settings are invalid")
	ErrConflict      = errors.New("system email settings changed concurrently")
	ErrUnavailable   = errors.New("system email settings are unavailable")
	ErrNotConfigured = errors.New("system email is not configured")
)

var passwordAAD = []byte("wenzwork:system-email:smtp-password:v1")

const (
	SourceDatabase     = "database"
	SourceLocal        = "local"
	SourceUnconfigured = "unconfigured"
)

type Config struct {
	Host       string
	Port       int
	Username   string
	Password   string
	From       string
	RequireTLS bool
	Timeout    time.Duration
}

type Settings struct {
	Configured             bool       `json:"configured"`
	Source                 string     `json:"source"`
	SMTPHost               string     `json:"smtpHost"`
	SMTPPort               int        `json:"smtpPort"`
	SMTPUser               string     `json:"smtpUser"`
	SMTPPasswordConfigured bool       `json:"smtpPasswordConfigured"`
	MailFrom               string     `json:"mailFrom"`
	Version                int64      `json:"version"`
	UpdatedAt              *time.Time `json:"updatedAt"`
}

type UpdateInput struct {
	SMTPHost          string
	SMTPPort          int
	SMTPUser          string
	SMTPPassword      *string
	ClearSMTPPassword bool
	MailFrom          string
	ExpectedVersion   int64
	ActorUserID       uuid.UUID
}

type TestInput struct {
	SMTPHost          string
	SMTPPort          int
	SMTPUser          string
	SMTPPassword      *string
	ClearSMTPPassword bool
	MailFrom          string
	Recipient         string
}

type settingsRow struct {
	Singleton              bool       `gorm:"column:singleton;primaryKey"`
	OverrideEnabled        bool       `gorm:"column:override_enabled"`
	SMTPHost               string     `gorm:"column:smtp_host"`
	SMTPPort               int        `gorm:"column:smtp_port"`
	SMTPUser               string     `gorm:"column:smtp_user"`
	SMTPPasswordCiphertext []byte     `gorm:"column:smtp_password_ciphertext"`
	MailFrom               string     `gorm:"column:mail_from"`
	Version                int64      `gorm:"column:version"`
	UpdatedBy              *uuid.UUID `gorm:"column:updated_by;type:uuid"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
}

func (settingsRow) TableName() string { return "system_email_settings" }

type auditLogRow struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	ActorUserID  *uuid.UUID `gorm:"column:actor_user_id"`
	Action       string     `gorm:"column:action"`
	ResourceType string     `gorm:"column:resource_type"`
	BeforeJSON   []byte     `gorm:"column:before_json;type:jsonb"`
	AfterJSON    []byte     `gorm:"column:after_json;type:jsonb"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
}

func (auditLogRow) TableName() string { return "audit_logs" }

type passwordCipher struct{ aead cipher.AEAD }

type Store struct {
	db       *gorm.DB
	fallback Config
	cipher   *passwordCipher
	now      func() time.Time
}

func NewStore(db *gorm.DB, fallback Config, encryptionKey string) (*Store, error) {
	if db == nil {
		return nil, errors.New("system email database is required")
	}
	codec, err := newPasswordCipher(encryptionKey)
	if err != nil {
		return nil, err
	}
	fallback = normalizeConfig(fallback)
	return &Store{db: db, fallback: fallback, cipher: codec, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (store *Store) GetSettings(ctx context.Context) (Settings, error) {
	row, err := store.loadRow(ctx)
	if err != nil {
		return Settings{}, err
	}
	config, source, err := store.configFromRow(row)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return Settings{}, err
	}
	return publicSettings(config, source, row), nil
}

// Send resolves settings for every delivery, so an administrator update takes
// effect without restarting any service that was given this Sender.
func (store *Store) Send(ctx context.Context, message mailer.Message) error {
	config, err := store.resolveForDelivery(ctx)
	if err != nil {
		return err
	}
	sender, err := newSender(config)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return sender.Send(ctx, message)
}

func (store *Store) Test(ctx context.Context, input TestInput) error {
	row, err := store.loadRow(ctx)
	if err != nil {
		return err
	}
	current, _, err := store.configFromRow(row)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return err
	}
	candidate, err := candidateConfig(current, input.SMTPHost, input.SMTPPort, input.SMTPUser,
		input.SMTPPassword, input.ClearSMTPPassword, input.MailFrom)
	if err != nil {
		return err
	}
	sender, err := newSender(candidate)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	recipient := strings.TrimSpace(input.Recipient)
	parsedRecipient, parseErr := mail.ParseAddress(recipient)
	if parseErr != nil || parsedRecipient.Address != recipient || strings.ContainsAny(recipient, "\r\n\x00") {
		return ErrInvalid
	}
	return sender.Send(ctx, mailer.Message{
		To: recipient, Subject: "WenzWork 系统邮箱测试",
		Text: "这是一封来自 WenzWork 管理后台的系统邮箱测试邮件。收到此邮件表示当前填写的 SMTP 配置可以正常投递。\n",
	})
}

func (store *Store) Update(ctx context.Context, input UpdateInput) (Settings, error) {
	if store == nil || store.db == nil || input.ExpectedVersion < 1 || input.ActorUserID == uuid.Nil {
		return Settings{}, ErrInvalid
	}
	var result Settings
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row settingsRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "singleton = ?", true).Error; err != nil {
			return fmt.Errorf("%w: lock settings: %v", ErrUnavailable, err)
		}
		if row.Version != input.ExpectedVersion {
			return ErrConflict
		}
		current, _, err := store.configFromRow(row)
		if err != nil && !errors.Is(err, ErrNotConfigured) {
			return err
		}
		candidate, err := candidateConfig(current, input.SMTPHost, input.SMTPPort, input.SMTPUser,
			input.SMTPPassword, input.ClearSMTPPassword, input.MailFrom)
		if err != nil {
			return err
		}
		before, _ := json.Marshal(publicSettings(current, sourceForRow(row, store.fallback), row))
		ciphertext, err := store.encryptPassword(candidate.Password)
		if err != nil {
			return err
		}
		now := store.now().UTC()
		row.OverrideEnabled = true
		row.SMTPHost, row.SMTPPort, row.SMTPUser = candidate.Host, candidate.Port, candidate.Username
		row.SMTPPasswordCiphertext, row.MailFrom = ciphertext, candidate.From
		row.Version++
		row.UpdatedBy, row.UpdatedAt = &input.ActorUserID, now
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("%w: update settings: %v", ErrUnavailable, err)
		}
		result = publicSettings(candidate, SourceDatabase, row)
		after, _ := json.Marshal(result)
		return createAudit(tx, input.ActorUserID, "system.email.update", before, after, now)
	})
	return result, err
}

func (store *Store) ResetToLocal(ctx context.Context, expectedVersion int64, actorUserID uuid.UUID) (Settings, error) {
	if store == nil || store.db == nil || expectedVersion < 1 || actorUserID == uuid.Nil {
		return Settings{}, ErrInvalid
	}
	var result Settings
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row settingsRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "singleton = ?", true).Error; err != nil {
			return fmt.Errorf("%w: lock settings: %v", ErrUnavailable, err)
		}
		if row.Version != expectedVersion {
			return ErrConflict
		}
		current, source, err := store.configFromRow(row)
		if err != nil && !errors.Is(err, ErrNotConfigured) {
			return err
		}
		before, _ := json.Marshal(publicSettings(current, source, row))
		now := store.now().UTC()
		row.OverrideEnabled = false
		row.SMTPHost, row.SMTPPort, row.SMTPUser = "", 0, ""
		row.SMTPPasswordCiphertext, row.MailFrom = nil, ""
		row.Version++
		row.UpdatedBy, row.UpdatedAt = &actorUserID, now
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("%w: reset settings: %v", ErrUnavailable, err)
		}
		result = publicSettings(store.fallback, sourceForConfig(store.fallback), row)
		after, _ := json.Marshal(result)
		return createAudit(tx, actorUserID, "system.email.reset_to_local", before, after, now)
	})
	return result, err
}

// EnsureDatabaseOverride imports a complete first-login SMTP configuration.
// Existing database settings are never overwritten by a restart or retry.
func (store *Store) EnsureDatabaseOverride(ctx context.Context, actorUserID uuid.UUID) error {
	if store == nil || store.db == nil || !validConfig(store.fallback) {
		return nil
	}
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row settingsRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "singleton = ?", true).Error; err != nil {
			return fmt.Errorf("%w: lock settings for import: %v", ErrUnavailable, err)
		}
		if row.OverrideEnabled {
			return nil
		}
		ciphertext, err := store.encryptPassword(store.fallback.Password)
		if err != nil {
			return err
		}
		now := store.now().UTC()
		row.OverrideEnabled = true
		row.SMTPHost, row.SMTPPort, row.SMTPUser = store.fallback.Host, store.fallback.Port, store.fallback.Username
		row.SMTPPasswordCiphertext, row.MailFrom = ciphertext, store.fallback.From
		if actorUserID != uuid.Nil {
			row.UpdatedBy = &actorUserID
		}
		row.UpdatedAt = now
		return tx.Save(&row).Error
	})
}

func (store *Store) resolveForDelivery(ctx context.Context) (Config, error) {
	row, err := store.loadRow(ctx)
	if err != nil {
		if validConfig(store.fallback) {
			return store.fallback, nil
		}
		return Config{}, err
	}
	config, _, err := store.configFromRow(row)
	return config, err
}

func (store *Store) loadRow(ctx context.Context) (settingsRow, error) {
	if store == nil || store.db == nil {
		return settingsRow{}, ErrUnavailable
	}
	var row settingsRow
	if err := store.db.WithContext(ctx).First(&row, "singleton = ?", true).Error; err != nil {
		return settingsRow{}, fmt.Errorf("%w: load settings: %v", ErrUnavailable, err)
	}
	return row, nil
}

func (store *Store) configFromRow(row settingsRow) (Config, string, error) {
	if !row.OverrideEnabled {
		if validConfig(store.fallback) {
			return store.fallback, SourceLocal, nil
		}
		return store.fallback, SourceUnconfigured, ErrNotConfigured
	}
	config := Config{Host: row.SMTPHost, Port: row.SMTPPort, Username: row.SMTPUser, From: row.MailFrom,
		RequireTLS: store.fallback.RequireTLS, Timeout: store.fallback.Timeout}
	if len(row.SMTPPasswordCiphertext) > 0 {
		plaintext, err := store.cipher.decrypt(row.SMTPPasswordCiphertext)
		if err != nil {
			return Config{}, SourceDatabase, fmt.Errorf("%w: decrypt SMTP password: %v", ErrUnavailable, err)
		}
		config.Password = string(plaintext)
		for index := range plaintext {
			plaintext[index] = 0
		}
	}
	config = normalizeConfig(config)
	if !validConfig(config) {
		return Config{}, SourceDatabase, ErrInvalid
	}
	return config, SourceDatabase, nil
}

func candidateConfig(current Config, host string, port int, username string, password *string, clearPassword bool, from string) (Config, error) {
	candidate := Config{Host: host, Port: port, Username: username, From: from,
		RequireTLS: current.RequireTLS, Timeout: current.Timeout, Password: current.Password}
	if clearPassword {
		candidate.Password = ""
	} else if password != nil {
		candidate.Password = *password
	}
	candidate = normalizeConfig(candidate)
	if password != nil && clearPassword || !validConfig(candidate) || (candidate.Username != "" && candidate.Password == "") {
		return Config{}, ErrInvalid
	}
	return candidate, nil
}

func normalizeConfig(config Config) Config {
	config.Host = strings.TrimSpace(config.Host)
	config.Username = strings.TrimSpace(config.Username)
	config.From = strings.TrimSpace(config.From)
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Second
	}
	return config
}

func validConfig(config Config) bool {
	return config.Host != "" && config.Port >= 1 && config.Port <= 65535 && config.From != "" &&
		!strings.ContainsAny(config.Host+config.Username+config.From, "\r\n\x00")
}

func newSender(config Config) (mailer.Sender, error) {
	if !validConfig(config) {
		return nil, ErrNotConfigured
	}
	return mailer.NewSMTPSender(mailer.SMTPConfig{Host: config.Host, Port: config.Port,
		Username: config.Username, Password: config.Password, From: config.From,
		RequireTLS: config.RequireTLS, Timeout: config.Timeout})
}

func sourceForConfig(config Config) string {
	if validConfig(config) {
		return SourceLocal
	}
	return SourceUnconfigured
}

func sourceForRow(row settingsRow, fallback Config) string {
	if row.OverrideEnabled {
		return SourceDatabase
	}
	return sourceForConfig(fallback)
}

func publicSettings(config Config, source string, row settingsRow) Settings {
	configured := validConfig(config)
	updatedAt := row.UpdatedAt.UTC()
	return Settings{Configured: configured, Source: source, SMTPHost: config.Host, SMTPPort: config.Port,
		SMTPUser: config.Username, SMTPPasswordConfigured: config.Password != "", MailFrom: config.From,
		Version: row.Version, UpdatedAt: &updatedAt}
}

func createAudit(tx *gorm.DB, actor uuid.UUID, action string, before, after []byte, now time.Time) error {
	if err := tx.Create(&auditLogRow{ID: uuid.New(), ActorUserID: &actor, Action: action,
		ResourceType: "system_email_settings", BeforeJSON: before, AfterJSON: after, CreatedAt: now}).Error; err != nil {
		return fmt.Errorf("%w: audit settings: %v", ErrUnavailable, err)
	}
	return nil
}

func (store *Store) encryptPassword(password string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}
	ciphertext, err := store.cipher.encrypt([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("%w: encrypt SMTP password: %v", ErrUnavailable, err)
	}
	return ciphertext, nil
}

func newPasswordCipher(masterKey string) (*passwordCipher, error) {
	if len(masterKey) < 32 {
		return nil, errors.New("system email encryption key must contain at least 32 bytes")
	}
	derivation := hmac.New(sha256.New, []byte(masterKey))
	_, _ = derivation.Write(passwordAAD)
	block, err := aes.NewCipher(derivation.Sum(nil))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &passwordCipher{aead: aead}, nil
}

func (codec *passwordCipher) encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, codec.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	result := make([]byte, 1, 1+len(nonce)+len(plaintext)+codec.aead.Overhead())
	result[0] = 1
	result = append(result, nonce...)
	return codec.aead.Seal(result, nonce, plaintext, passwordAAD), nil
}

func (codec *passwordCipher) decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 1+codec.aead.NonceSize()+codec.aead.Overhead() || ciphertext[0] != 1 {
		return nil, errors.New("SMTP password ciphertext is invalid")
	}
	nonceEnd := 1 + codec.aead.NonceSize()
	return codec.aead.Open(nil, ciphertext[1:nonceEnd], ciphertext[nonceEnd:], passwordAAD)
}
