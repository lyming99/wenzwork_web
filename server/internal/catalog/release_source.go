package catalog

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
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrReleaseSourceInvalid           = errors.New("release source settings are invalid")
	ErrReleaseSourceConflict          = errors.New("release source settings changed concurrently")
	ErrReleaseSourceSecretUnavailable = errors.New("release source secret encryption is unavailable")

	githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	releaseSourceTokenAAD   = []byte("wenzwork:release-source:github-token:v1")
)

const maxGitHubTokenLength = 1000
const maxReleaseMirrorBaseURLLength = 2048

const (
	ReleaseProjectWeb     = "web"
	ReleaseProjectDesktop = "desktop"
	ReleaseProjectMobile  = "mobile"
)

type ReleaseSourceSettings struct {
	Project               string    `json:"project"`
	GitHubRepository      string    `json:"githubRepository"`
	GitHubTokenConfigured bool      `json:"githubTokenConfigured"`
	MirrorBaseURL         string    `json:"mirrorBaseUrl"`
	Version               int64     `json:"version"`
	UpdatedAt             time.Time `json:"updatedAt"`
}

type ReleaseSourceCredentials struct {
	Project          string
	GitHubRepository string
	GitHubToken      string
	MirrorBaseURL    string
}

type UpdateReleaseSourceSettingsInput struct {
	Project          string
	GitHubRepository string
	GitHubToken      *string
	ClearGitHubToken bool
	MirrorBaseURL    string
	ExpectedVersion  int64
	ActorUserID      uuid.UUID
}

type releaseSourceSettingsRow struct {
	Project                string     `gorm:"column:project;primaryKey"`
	GitHubRepository       string     `gorm:"column:github_repository"`
	GitHubTokenCiphertext  []byte     `gorm:"column:github_token_ciphertext"`
	GitHubTokenInitialized bool       `gorm:"column:github_token_initialized"`
	MirrorBaseURL          string     `gorm:"column:mirror_base_url"`
	Version                int64      `gorm:"column:version"`
	UpdatedBy              *uuid.UUID `gorm:"column:updated_by;type:uuid"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
}

func (releaseSourceSettingsRow) TableName() string { return "release_source_settings" }

type releaseSourceTokenCipher struct {
	aead cipher.AEAD
}

func WithReleaseSourceTokenEncryptionKey(masterKey string) StoreOption {
	return func(store *Store) error {
		codec, err := newReleaseSourceTokenCipher(masterKey)
		if err != nil {
			return err
		}
		store.releaseSourceTokenCipher = codec
		return nil
	}
}

func (s *Store) GetReleaseSourceSettings(ctx context.Context) (ReleaseSourceSettings, error) {
	return s.GetReleaseSourceSettingsForProject(ctx, ReleaseProjectWeb)
}

func (s *Store) GetReleaseSourceSettingsForProject(ctx context.Context, project string) (ReleaseSourceSettings, error) {
	row, err := s.getReleaseSourceSettingsRow(ctx, project)
	if err != nil {
		return ReleaseSourceSettings{}, err
	}
	return releaseSourceSettingsFromRow(row), nil
}

func (s *Store) ListReleaseSourceSettings(ctx context.Context) ([]ReleaseSourceSettings, error) {
	var rows []releaseSourceSettingsRow
	if err := s.db.WithContext(ctx).Order("CASE project WHEN 'web' THEN 1 WHEN 'desktop' THEN 2 ELSE 3 END").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list release source settings: %w", err)
	}
	items := make([]ReleaseSourceSettings, 0, len(rows))
	for _, row := range rows {
		items = append(items, releaseSourceSettingsFromRow(row))
	}
	return items, nil
}

func (s *Store) GetReleaseSourceCredentials(ctx context.Context) (ReleaseSourceCredentials, error) {
	return s.GetReleaseSourceCredentialsForProject(ctx, ReleaseProjectWeb)
}

func (s *Store) GetReleaseSourceCredentialsForProject(ctx context.Context, project string) (ReleaseSourceCredentials, error) {
	row, err := s.getReleaseSourceSettingsRow(ctx, project)
	if err != nil {
		return ReleaseSourceCredentials{}, err
	}
	return s.releaseSourceCredentialsFromRow(row)
}

func (s *Store) releaseSourceCredentialsFromRow(row releaseSourceSettingsRow) (ReleaseSourceCredentials, error) {
	credentials := ReleaseSourceCredentials{
		Project: row.Project, GitHubRepository: row.GitHubRepository, MirrorBaseURL: row.MirrorBaseURL,
	}
	if len(row.GitHubTokenCiphertext) == 0 {
		return credentials, nil
	}
	if s.releaseSourceTokenCipher == nil {
		return ReleaseSourceCredentials{}, ErrReleaseSourceSecretUnavailable
	}
	plaintext, err := s.releaseSourceTokenCipher.decrypt(row.GitHubTokenCiphertext)
	if err != nil {
		return ReleaseSourceCredentials{}, fmt.Errorf("decrypt GitHub release token: %w", err)
	}
	credentials.GitHubToken = string(plaintext)
	for index := range plaintext {
		plaintext[index] = 0
	}
	return credentials, nil
}

func (s *Store) GetReleaseSourceCredentialsByRepository(ctx context.Context, repository string) (ReleaseSourceCredentials, error) {
	repository = strings.TrimSpace(repository)
	if !validGitHubRepository(repository) {
		return ReleaseSourceCredentials{}, ErrReleaseSourceInvalid
	}
	var row releaseSourceSettingsRow
	if err := s.db.WithContext(ctx).First(&row, "github_repository = ?", repository).Error; err != nil {
		return ReleaseSourceCredentials{}, fmt.Errorf("load release source credentials by repository: %w", err)
	}
	return s.releaseSourceCredentialsFromRow(row)
}

func (s *Store) getReleaseSourceSettingsRow(ctx context.Context, project string) (releaseSourceSettingsRow, error) {
	project = strings.ToLower(strings.TrimSpace(project))
	if !ValidReleaseProject(project) {
		return releaseSourceSettingsRow{}, ErrReleaseSourceInvalid
	}
	var row releaseSourceSettingsRow
	if err := s.db.WithContext(ctx).First(&row, "project = ?", project).Error; err != nil {
		return releaseSourceSettingsRow{}, fmt.Errorf("load release source settings: %w", err)
	}
	return row, nil
}

// EnsureReleaseSourceSettings seeds an empty settings row during an upgrade.
// The legacy environment token is considered exactly once, so clearing a
// database-backed token cannot be undone by a later restart.
func (s *Store) EnsureReleaseSourceSettings(ctx context.Context, repository, legacyToken string) error {
	return s.EnsureReleaseProjectSourceSettings(ctx, ReleaseProjectWeb, repository, legacyToken)
}

func (s *Store) EnsureReleaseProjectSourceSettings(ctx context.Context, project, repository, legacyToken string) error {
	project = strings.ToLower(strings.TrimSpace(project))
	repository = strings.TrimSpace(repository)
	if !ValidReleaseProject(project) || !validGitHubRepository(repository) {
		return ErrReleaseSourceInvalid
	}
	legacyToken = strings.TrimSpace(legacyToken)
	var encryptedToken []byte
	if legacyToken != "" {
		if !validGitHubToken(legacyToken) {
			return ErrReleaseSourceInvalid
		}
		if s.releaseSourceTokenCipher == nil {
			return ErrReleaseSourceSecretUnavailable
		}
		var err error
		encryptedToken, err = s.releaseSourceTokenCipher.encrypt([]byte(legacyToken))
		if err != nil {
			return fmt.Errorf("encrypt legacy GitHub release token: %w", err)
		}
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current releaseSourceSettingsRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "project = ?", project).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("lock release source settings for seed: %w", err)
			}
			current = releaseSourceSettingsRow{Project: project, GitHubRepository: repository, Version: 1, UpdatedAt: time.Now().UTC()}
			if len(encryptedToken) > 0 {
				current.GitHubTokenCiphertext = encryptedToken
			}
			current.GitHubTokenInitialized = true
			var duplicateCount int64
			if err := tx.Model(&releaseSourceSettingsRow{}).
				Where("project <> ? AND github_repository = ?", project, repository).
				Count(&duplicateCount).Error; err != nil {
				return fmt.Errorf("check release source seed repository: %w", err)
			}
			if duplicateCount > 0 {
				return ErrReleaseSourceInvalid
			}
			if err := tx.Create(&current).Error; err != nil {
				return fmt.Errorf("create release source settings seed: %w", err)
			}
			return nil
		}
		changed := false
		if current.GitHubRepository == "" {
			current.GitHubRepository = repository
			changed = true
		}
		if !current.GitHubTokenInitialized {
			if len(encryptedToken) > 0 {
				current.GitHubTokenCiphertext = encryptedToken
			}
			current.GitHubTokenInitialized = true
			changed = true
		}
		if !validGitHubRepository(current.GitHubRepository) {
			return ErrReleaseSourceInvalid
		}
		var duplicateCount int64
		if err := tx.Model(&releaseSourceSettingsRow{}).
			Where("project <> ? AND github_repository = ?", project, current.GitHubRepository).
			Count(&duplicateCount).Error; err != nil {
			return fmt.Errorf("check release source seed repository: %w", err)
		}
		if duplicateCount > 0 {
			return ErrReleaseSourceInvalid
		}
		if changed {
			current.UpdatedAt = time.Now().UTC()
			if err := tx.Save(&current).Error; err != nil {
				return fmt.Errorf("seed release source settings: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) UpdateReleaseSourceSettings(ctx context.Context, input UpdateReleaseSourceSettingsInput) (ReleaseSourceSettings, error) {
	input.Project = strings.ToLower(strings.TrimSpace(input.Project))
	if input.Project == "" {
		input.Project = ReleaseProjectWeb
	}
	input.GitHubRepository = strings.TrimSpace(input.GitHubRepository)
	var mirrorValid bool
	input.MirrorBaseURL, mirrorValid = normalizeReleaseMirrorBaseURL(input.MirrorBaseURL)
	if input.ActorUserID == uuid.Nil || input.ExpectedVersion < 1 || !ValidReleaseProject(input.Project) ||
		!validGitHubRepository(input.GitHubRepository) || !mirrorValid {
		return ReleaseSourceSettings{}, ErrReleaseSourceInvalid
	}

	var encryptedToken []byte
	replaceToken := false
	if input.GitHubToken != nil {
		token := strings.TrimSpace(*input.GitHubToken)
		if token != "" {
			if !validGitHubToken(token) || input.ClearGitHubToken {
				return ReleaseSourceSettings{}, ErrReleaseSourceInvalid
			}
			if s.releaseSourceTokenCipher == nil {
				return ReleaseSourceSettings{}, ErrReleaseSourceSecretUnavailable
			}
			var err error
			encryptedToken, err = s.releaseSourceTokenCipher.encrypt([]byte(token))
			if err != nil {
				return ReleaseSourceSettings{}, fmt.Errorf("encrypt GitHub release token: %w", err)
			}
			replaceToken = true
		}
	}
	if input.ClearGitHubToken && replaceToken {
		return ReleaseSourceSettings{}, ErrReleaseSourceInvalid
	}

	var result ReleaseSourceSettings
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current releaseSourceSettingsRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "project = ?", input.Project).Error; err != nil {
			return fmt.Errorf("lock release source settings: %w", err)
		}
		if current.Version != input.ExpectedVersion {
			return ErrReleaseSourceConflict
		}
		var duplicateCount int64
		if err := tx.Model(&releaseSourceSettingsRow{}).
			Where("project <> ? AND github_repository = ?", input.Project, input.GitHubRepository).
			Count(&duplicateCount).Error; err != nil {
			return fmt.Errorf("check duplicate release repository: %w", err)
		}
		if duplicateCount > 0 {
			return ErrReleaseSourceInvalid
		}

		beforeJSON, _ := json.Marshal(releaseSourceSettingsFromRow(current))
		now := time.Now().UTC()
		current.GitHubRepository = input.GitHubRepository
		current.MirrorBaseURL = input.MirrorBaseURL
		switch {
		case input.ClearGitHubToken:
			current.GitHubTokenCiphertext = nil
		case replaceToken:
			current.GitHubTokenCiphertext = encryptedToken
		}
		current.GitHubTokenInitialized = true
		current.Version++
		current.UpdatedBy = &input.ActorUserID
		current.UpdatedAt = now
		if err := tx.Save(&current).Error; err != nil {
			return fmt.Errorf("update release source settings: %w", err)
		}

		result = releaseSourceSettingsFromRow(current)
		afterJSON, _ := json.Marshal(result)
		audit := catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "release.source.update",
			ResourceType: "release_source_settings", BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("audit release source settings update: %w", err)
		}
		return nil
	})
	return result, err
}

func validGitHubRepository(repository string) bool {
	repository = strings.TrimSpace(repository)
	return len(repository) <= 200 && githubRepositoryPattern.MatchString(repository) && !strings.Contains(repository, "..")
}

func normalizeReleaseMirrorBaseURL(value string) (string, bool) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "", true
	}
	if len(value) > maxReleaseMirrorBaseURLLength {
		return "", false
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", false
	}
	cleanPath := strings.TrimRight(parsed.Path, "/")
	if cleanPath != "" && path.Clean(cleanPath) != cleanPath {
		return "", false
	}
	parsed.Path = cleanPath
	return parsed.String(), true
}

func ValidReleaseProject(project string) bool {
	switch strings.ToLower(strings.TrimSpace(project)) {
	case ReleaseProjectWeb, ReleaseProjectDesktop, ReleaseProjectMobile:
		return true
	default:
		return false
	}
}

func validGitHubToken(token string) bool {
	if token == "" || len(token) > maxGitHubTokenLength || !utf8.ValidString(token) {
		return false
	}
	for _, character := range token {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func releaseSourceSettingsFromRow(row releaseSourceSettingsRow) ReleaseSourceSettings {
	return ReleaseSourceSettings{
		Project:               row.Project,
		GitHubRepository:      row.GitHubRepository,
		GitHubTokenConfigured: len(row.GitHubTokenCiphertext) > 0,
		MirrorBaseURL:         row.MirrorBaseURL,
		Version:               row.Version,
		UpdatedAt:             row.UpdatedAt.UTC(),
	}
}

func newReleaseSourceTokenCipher(masterKey string) (*releaseSourceTokenCipher, error) {
	if len(masterKey) < 32 {
		return nil, errors.New("release source token encryption key must contain at least 32 bytes")
	}
	derivation := hmac.New(sha256.New, []byte(masterKey))
	_, _ = derivation.Write(releaseSourceTokenAAD)
	block, err := aes.NewCipher(derivation.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create release source token cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create release source token AEAD: %w", err)
	}
	return &releaseSourceTokenCipher{aead: aead}, nil
}

func (c *releaseSourceTokenCipher) encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate release source token nonce: %w", err)
	}
	result := make([]byte, 1, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	result[0] = 1
	result = append(result, nonce...)
	return c.aead.Seal(result, nonce, plaintext, releaseSourceTokenAAD), nil
}

func (c *releaseSourceTokenCipher) decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 1+c.aead.NonceSize()+c.aead.Overhead() || ciphertext[0] != 1 {
		return nil, errors.New("release source token ciphertext is invalid")
	}
	nonceEnd := 1 + c.aead.NonceSize()
	plaintext, err := c.aead.Open(nil, ciphertext[1:nonceEnd], ciphertext[nonceEnd:], releaseSourceTokenAAD)
	if err != nil {
		return nil, errors.New("release source token authentication failed")
	}
	return plaintext, nil
}
