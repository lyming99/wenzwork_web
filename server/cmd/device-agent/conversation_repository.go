package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const (
	conversationDatabaseDirectoryExtension = ".chat"
	conversationDatabaseFilename           = "conversations.sqlite3"
	defaultConversationJournalRetention    = 4096
	maximumConversationPageSize            = 200
	maximumConversationChangePageSize      = 200
)

var (
	errConversationNotFound = errors.New("conversation is not found")
	errConversationConflict = errors.New("conversation revision conflict")
	errConversationInvalid  = errors.New("conversation input is invalid")
)

// ConversationRepository is the device-local source of truth for chat data.
// The interface deliberately mirrors WenzMark's split conversation/message
// store rather than exposing the agent's identity/configuration state file.
type ConversationRepository interface {
	Close() error
	ImportLegacy(context.Context, map[string]conversation) error
	ListConversations(context.Context, conversationListOptions) (conversationPage, error)
	GetConversation(context.Context, string) (conversationView, error)
	CreateConversation(context.Context, string, string, time.Time) (conversationView, error)
	DeleteConversation(context.Context, string, time.Time) (conversationChange, error)
	ListMessages(context.Context, messageListOptions) (messagePage, error)
	MessagesForCompletion(context.Context, string) ([]chatMessage, conversationView, error)
	BeginTurn(context.Context, string, string, time.Time) (conversationTurn, error)
	AppendAssistantChunk(context.Context, string, string, string) (messageChange, error)
	FinishTurn(context.Context, string, string, string, time.Time) (messageChange, conversationChange, error)
	AbortTurn(context.Context, string, string, string, time.Time) (messageChange, conversationChange, error)
}

type conversationListOptions struct {
	Cursor        string
	Limit         int
	AfterSequence *uint64
}

type messageListOptions struct {
	ConversationID string
	Cursor         string
	Limit          int
	AfterSequence  *uint64
}

type conversationChange struct {
	Sequence uint64           `json:"sequence"`
	Deleted  bool             `json:"deleted,omitempty"`
	Value    conversationView `json:"value"`
}

type messageChange struct {
	Sequence uint64      `json:"sequence"`
	Deleted  bool        `json:"deleted,omitempty"`
	Value    chatMessage `json:"value"`
}

type conversationPage struct {
	Items          []conversationView   `json:"items"`
	Changes        []conversationChange `json:"changes"`
	NextCursor     *string              `json:"nextCursor"`
	HighWatermark  uint64               `json:"highWatermark"`
	LatestSequence uint64               `json:"latestSequence"`
	ResetRequired  bool                 `json:"resetRequired"`
	HasMoreChanges bool                 `json:"hasMoreChanges"`
}

type messagePage struct {
	Items          []chatMessage   `json:"items"`
	Changes        []messageChange `json:"changes"`
	NextCursor     *string         `json:"nextCursor"`
	HighWatermark  uint64          `json:"highWatermark"`
	LatestSequence uint64          `json:"latestSequence"`
	ResetRequired  bool            `json:"resetRequired"`
	HasMoreChanges bool            `json:"hasMoreChanges"`
}

type conversationTurn struct {
	History      []chatMessage
	User         messageChange
	Assistant    messageChange
	Conversation conversationChange
}

type sqliteConversationRepository struct {
	db               *sql.DB
	path             string
	cursorKey        []byte
	journalRetention int
}

type conversationCursor struct {
	Version   int    `json:"v"`
	Kind      string `json:"k"`
	Scope     string `json:"s"`
	Snapshot  uint64 `json:"w"`
	UpdatedAt int64  `json:"u,omitempty"`
	ID        string `json:"i,omitempty"`
	BeforeSeq uint64 `json:"b,omitempty"`
}

func conversationDatabasePath(statePath string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(statePath))
	if cleaned == "" || cleaned == "." || !filepath.IsAbs(cleaned) {
		return "", errConversationInvalid
	}
	directory := cleaned + conversationDatabaseDirectoryExtension
	return filepath.Join(directory, conversationDatabaseFilename), nil
}

func openConversationRepository(statePath string, cursorKey []byte) (*sqliteConversationRepository, error) {
	return openConversationRepositoryWithRetention(statePath, cursorKey, defaultConversationJournalRetention)
}

func openConversationRepositoryWithRetention(statePath string, cursorKey []byte, retention int) (*sqliteConversationRepository, error) {
	path, err := conversationDatabasePath(statePath)
	if err != nil || len(cursorKey) < 32 || retention < 2 {
		return nil, errConversationInvalid
	}
	if err := preparePrivateConversationDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := rejectUnsafeConversationFile(path); err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open conversation database: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	repository := &sqliteConversationRepository{
		db: database, path: path, cursorKey: append([]byte(nil), cursorKey...), journalRetention: retention,
	}
	if err := repository.initialize(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return repository, nil
}

func (repository *sqliteConversationRepository) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA trusted_schema = OFF`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id TEXT PRIMARY KEY NOT NULL,
			title TEXT NOT NULL,
			model TEXT NOT NULL,
			revision INTEGER NOT NULL CHECK (revision >= 0),
			state TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0 CHECK (message_count >= 0),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			active_assistant_id TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS conversations_updated_idx ON conversations(updated_at DESC, id DESC)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY NOT NULL,
			conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL CHECK (seq > 0),
			revision INTEGER NOT NULL CHECK (revision >= 0),
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE(conversation_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS messages_conversation_seq_idx ON messages(conversation_id, seq DESC)`,
		`CREATE TABLE IF NOT EXISTS change_journal (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			stream TEXT NOT NULL,
			scope TEXT NOT NULL,
			record_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			deleted INTEGER NOT NULL DEFAULT 0,
			payload BLOB NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS change_journal_stream_scope_seq_idx ON change_journal(stream, scope, sequence)`,
		`CREATE TABLE IF NOT EXISTS conversation_metadata (key TEXT PRIMARY KEY NOT NULL, value TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := repository.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize conversation database: %w", err)
		}
	}
	if err := secureConversationDatabaseFiles(repository.path); err != nil {
		return err
	}
	return nil
}

func (repository *sqliteConversationRepository) Close() error {
	if repository == nil || repository.db == nil {
		return nil
	}
	_, _ = repository.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	err := repository.db.Close()
	if secureErr := secureConversationDatabaseFiles(repository.path); err == nil {
		err = secureErr
	}
	return err
}

func normalizeConversationLimit(value int) (int, error) {
	if value == 0 {
		return 50, nil
	}
	if value < 1 || value > maximumConversationPageSize {
		return 0, errConversationInvalid
	}
	return value, nil
}

func (repository *sqliteConversationRepository) ImportLegacy(ctx context.Context, legacy map[string]conversation) error {
	if len(legacy) == 0 {
		return nil
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var migrated string
	err = tx.QueryRowContext(ctx, `SELECT value FROM conversation_metadata WHERE key = 'legacy_state_import_v1'`).Scan(&migrated)
	if err == nil && migrated == "complete" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, value := range legacy {
		if err := validateLegacyConversation(value); err != nil {
			return err
		}
		createdAt := value.UpdatedAt.UTC().UnixNano()
		if len(value.Messages) > 0 {
			createdAt = value.Messages[0].CreatedAt.UTC().UnixNano()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversations(id, title, model, revision, state, message_count, created_at, updated_at, active_assistant_id)
			VALUES(?, ?, ?, 0, ?, ?, ?, ?, '')
			ON CONFLICT(id) DO UPDATE SET title=excluded.title, model=excluded.model, state=excluded.state,
				message_count=excluded.message_count, updated_at=excluded.updated_at`,
			value.ID, value.Title, value.Model, normalizedConversationState(value.State), len(value.Messages), createdAt, value.UpdatedAt.UTC().UnixNano()); err != nil {
			return err
		}
		for index, message := range value.Messages {
			sequence := message.Sequence
			if sequence == 0 {
				sequence = uint64(index + 1)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO messages(id, conversation_id, seq, revision, role, content, status, created_at)
				VALUES(?, ?, ?, 0, ?, ?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET conversation_id=excluded.conversation_id, seq=excluded.seq,
					role=excluded.role, content=excluded.content, status=excluded.status, created_at=excluded.created_at`,
				message.ID, value.ID, sequence, message.Role, message.Content, normalizedMessageStatus(message.Status), message.CreatedAt.UTC().UnixNano()); err != nil {
				return err
			}
		}
		view := value.view()
		view.State = normalizedConversationState(view.State)
		journalSequence, err := appendJournal(ctx, tx, "conversation", "", value.ID, false, view, value.UpdatedAt)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conversations SET revision = ? WHERE id = ?`, journalSequence, value.ID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_metadata(key, value) VALUES('legacy_state_import_v1', 'complete')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := repository.db.ExecContext(ctx, `PRAGMA wal_checkpoint(FULL)`); err != nil {
		return err
	}
	if err := syncConversationDatabaseFiles(repository.path); err != nil {
		return err
	}
	return repository.verifyLegacyImport(ctx, legacy)
}

func validateLegacyConversation(value conversation) error {
	if uuid.Validate(value.ID) != nil || strings.TrimSpace(value.Title) == "" || len(value.Title) > 200 || len(value.Model) > 120 || value.UpdatedAt.IsZero() {
		return errConversationInvalid
	}
	seenIDs := make(map[string]struct{}, len(value.Messages))
	seenSequences := make(map[uint64]struct{}, len(value.Messages))
	for index, message := range value.Messages {
		sequence := message.Sequence
		if sequence == 0 {
			sequence = uint64(index + 1)
		}
		if uuid.Validate(message.ID) != nil || !validMessageRole(message.Role) || !utf8.ValidString(message.Content) || message.CreatedAt.IsZero() {
			return errConversationInvalid
		}
		if _, exists := seenIDs[message.ID]; exists {
			return errConversationInvalid
		}
		if _, exists := seenSequences[sequence]; exists {
			return errConversationInvalid
		}
		seenIDs[message.ID] = struct{}{}
		seenSequences[sequence] = struct{}{}
	}
	return nil
}

func (repository *sqliteConversationRepository) verifyLegacyImport(ctx context.Context, legacy map[string]conversation) error {
	for _, value := range legacy {
		var messageCount int
		if err := repository.db.QueryRowContext(ctx, `SELECT message_count FROM conversations WHERE id = ?`, value.ID).Scan(&messageCount); err != nil || messageCount != len(value.Messages) {
			return errors.New("verify legacy conversation import")
		}
		var stored int
		if err := repository.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, value.ID).Scan(&stored); err != nil || stored != len(value.Messages) {
			return errors.New("verify legacy message import")
		}
	}
	return nil
}

func (repository *sqliteConversationRepository) ListConversations(ctx context.Context, options conversationListOptions) (conversationPage, error) {
	limit, err := normalizeConversationLimit(options.Limit)
	if err != nil || options.AfterSequence != nil && options.Cursor != "" {
		return conversationPage{}, errConversationInvalid
	}
	if options.AfterSequence != nil {
		return repository.conversationChanges(ctx, *options.AfterSequence, limit)
	}
	return repository.conversationSnapshot(ctx, options.Cursor, limit, false)
}

func (repository *sqliteConversationRepository) conversationSnapshot(ctx context.Context, rawCursor string, limit int, reset bool) (conversationPage, error) {
	latest, err := repository.journalHighWatermark(ctx, "conversation", "")
	if err != nil {
		return conversationPage{}, err
	}
	cursor := conversationCursor{Version: 1, Kind: "conversations", Scope: "", Snapshot: latest}
	if rawCursor != "" {
		decoded, err := repository.decodeCursor(rawCursor, "conversations", "")
		if err != nil || decoded.UpdatedAt == 0 || decoded.ID == "" || decoded.BeforeSeq != 0 {
			return conversationPage{}, errConversationInvalid
		}
		cursor = decoded
		latest = cursor.Snapshot
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT id, title, model, revision, state, message_count, updated_at
		FROM conversations
		WHERE revision <= ? AND (? = 0 OR (updated_at, id) < (?, ?))
		ORDER BY updated_at DESC, id DESC LIMIT ?`, cursor.Snapshot, cursor.UpdatedAt, cursor.UpdatedAt, cursor.ID, limit+1)
	if err != nil {
		return conversationPage{}, err
	}
	defer rows.Close()
	items := make([]conversationView, 0, limit)
	for rows.Next() {
		var value conversationView
		var updated int64
		if err := rows.Scan(&value.ID, &value.Title, &value.Model, &value.Revision, &value.State, &value.MessageCount, &updated); err != nil {
			return conversationPage{}, err
		}
		value.UpdatedAt = time.Unix(0, updated).UTC()
		items = append(items, value)
	}
	if err := rows.Err(); err != nil {
		return conversationPage{}, err
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		encoded, err := repository.encodeCursor(conversationCursor{
			Version: 1, Kind: "conversations", Scope: "", Snapshot: cursor.Snapshot,
			UpdatedAt: last.UpdatedAt.UnixNano(), ID: last.ID,
		})
		if err != nil {
			return conversationPage{}, err
		}
		next = &encoded
	}
	return conversationPage{
		Items: items, Changes: []conversationChange{}, NextCursor: next,
		HighWatermark: cursor.Snapshot, LatestSequence: latest, ResetRequired: reset,
	}, nil
}

func (repository *sqliteConversationRepository) conversationChanges(ctx context.Context, after uint64, limit int) (conversationPage, error) {
	minimum, latest, err := repository.journalBounds(ctx, "conversation", "")
	if err != nil {
		return conversationPage{}, err
	}
	if after > latest {
		return conversationPage{}, errConversationInvalid
	}
	if minimum > 0 && after+1 < minimum {
		return repository.conversationSnapshot(ctx, "", limit, true)
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT sequence, deleted, payload FROM change_journal
		WHERE stream = 'conversation' AND scope = '' AND sequence > ? ORDER BY sequence LIMIT ?`, after, maximumConversationChangePageSize+1)
	if err != nil {
		return conversationPage{}, err
	}
	defer rows.Close()
	changes := make([]conversationChange, 0, maximumConversationChangePageSize)
	for rows.Next() {
		var change conversationChange
		var deleted int
		var payload []byte
		if err := rows.Scan(&change.Sequence, &deleted, &payload); err != nil || json.Unmarshal(payload, &change.Value) != nil {
			return conversationPage{}, errors.New("invalid conversation journal")
		}
		change.Deleted = deleted != 0
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return conversationPage{}, err
	}
	hasMore := len(changes) > maximumConversationChangePageSize
	if hasMore {
		changes = changes[:maximumConversationChangePageSize]
	}
	watermark := latest
	if hasMore && len(changes) > 0 {
		watermark = changes[len(changes)-1].Sequence
	}
	return conversationPage{
		Items: []conversationView{}, Changes: changes, HighWatermark: watermark,
		LatestSequence: latest, HasMoreChanges: hasMore,
	}, nil
}

func (repository *sqliteConversationRepository) GetConversation(ctx context.Context, id string) (conversationView, error) {
	if uuid.Validate(id) != nil {
		return conversationView{}, errConversationInvalid
	}
	var value conversationView
	var updated int64
	err := repository.db.QueryRowContext(ctx, `
		SELECT id, title, model, revision, state, message_count, updated_at FROM conversations WHERE id = ?`, id).
		Scan(&value.ID, &value.Title, &value.Model, &value.Revision, &value.State, &value.MessageCount, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return conversationView{}, errConversationNotFound
	}
	if err != nil {
		return conversationView{}, err
	}
	value.UpdatedAt = time.Unix(0, updated).UTC()
	return value, nil
}

func (repository *sqliteConversationRepository) CreateConversation(ctx context.Context, title, model string, now time.Time) (conversationView, error) {
	title, model, now = strings.TrimSpace(title), strings.TrimSpace(model), now.UTC()
	if title == "" || len(title) > 200 || len(model) > 120 || now.IsZero() {
		return conversationView{}, errConversationInvalid
	}
	id := uuid.NewString()
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return conversationView{}, err
	}
	defer tx.Rollback()
	value := conversationView{ID: id, Title: title, Model: model, UpdatedAt: now, State: "idle"}
	sequence, err := appendJournal(ctx, tx, "conversation", "", id, false, value, now)
	if err != nil {
		return conversationView{}, err
	}
	value.Revision = sequence
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversations(id, title, model, revision, state, message_count, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'idle', 0, ?, ?)`, id, title, model, sequence, now.UnixNano(), now.UnixNano()); err != nil {
		return conversationView{}, err
	}
	if err := updateJournalPayload(ctx, tx, sequence, value); err != nil {
		return conversationView{}, err
	}
	if err := repository.commitMutation(ctx, tx, "conversation", ""); err != nil {
		return conversationView{}, err
	}
	return value, nil
}

func (repository *sqliteConversationRepository) DeleteConversation(ctx context.Context, id string, now time.Time) (conversationChange, error) {
	value, err := repository.GetConversation(ctx, id)
	if err != nil {
		return conversationChange{}, err
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return conversationChange{}, err
	}
	defer tx.Rollback()
	value.UpdatedAt, value.MessageCount, value.State = now.UTC(), 0, "idle"
	sequence, err := appendJournal(ctx, tx, "conversation", "", id, true, value, now)
	if err != nil {
		return conversationChange{}, err
	}
	value.Revision = sequence
	if err := updateJournalPayload(ctx, tx, sequence, value); err != nil {
		return conversationChange{}, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND revision = ?`, id, valueRevisionBeforeDelete(value, sequence))
	if err != nil {
		return conversationChange{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return conversationChange{}, errConversationConflict
	}
	if err := repository.commitMutation(ctx, tx, "conversation", ""); err != nil {
		return conversationChange{}, err
	}
	return conversationChange{Sequence: sequence, Deleted: true, Value: value}, nil
}

func valueRevisionBeforeDelete(value conversationView, tombstoneSequence uint64) uint64 {
	// DeleteConversation replaces the public revision with the tombstone's
	// sequence. The row still carries the revision read before the tombstone.
	if value.Revision == tombstoneSequence {
		return 0
	}
	return value.Revision
}

func (repository *sqliteConversationRepository) ListMessages(ctx context.Context, options messageListOptions) (messagePage, error) {
	limit, err := normalizeConversationLimit(options.Limit)
	if err != nil || uuid.Validate(options.ConversationID) != nil || options.AfterSequence != nil && options.Cursor != "" {
		return messagePage{}, errConversationInvalid
	}
	if _, err := repository.GetConversation(ctx, options.ConversationID); err != nil {
		return messagePage{}, err
	}
	if options.AfterSequence != nil {
		return repository.messageChanges(ctx, options.ConversationID, *options.AfterSequence, limit)
	}
	return repository.messageSnapshot(ctx, options.ConversationID, options.Cursor, limit, false)
}

func (repository *sqliteConversationRepository) messageSnapshot(ctx context.Context, conversationID, rawCursor string, limit int, reset bool) (messagePage, error) {
	latest, err := repository.journalHighWatermark(ctx, "message", conversationID)
	if err != nil {
		return messagePage{}, err
	}
	cursor := conversationCursor{Version: 1, Kind: "messages", Scope: conversationID, Snapshot: latest}
	if rawCursor != "" {
		decoded, err := repository.decodeCursor(rawCursor, "messages", conversationID)
		if err != nil || decoded.BeforeSeq == 0 || decoded.UpdatedAt != 0 || decoded.ID != "" {
			return messagePage{}, errConversationInvalid
		}
		cursor = decoded
		latest = cursor.Snapshot
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT id, revision, seq, role, content, created_at, status
		FROM messages WHERE conversation_id = ? AND revision <= ? AND (? = 0 OR seq < ?)
		ORDER BY seq DESC LIMIT ?`, conversationID, cursor.Snapshot, cursor.BeforeSeq, cursor.BeforeSeq, limit+1)
	if err != nil {
		return messagePage{}, err
	}
	defer rows.Close()
	newestFirst := make([]chatMessage, 0, limit)
	for rows.Next() {
		value, err := scanMessage(rows)
		if err != nil {
			return messagePage{}, err
		}
		newestFirst = append(newestFirst, value)
	}
	if err := rows.Err(); err != nil {
		return messagePage{}, err
	}
	var next *string
	if len(newestFirst) > limit {
		newestFirst = newestFirst[:limit]
		last := newestFirst[len(newestFirst)-1]
		encoded, err := repository.encodeCursor(conversationCursor{
			Version: 1, Kind: "messages", Scope: conversationID, Snapshot: cursor.Snapshot, BeforeSeq: last.Sequence,
		})
		if err != nil {
			return messagePage{}, err
		}
		next = &encoded
	}
	items := make([]chatMessage, len(newestFirst))
	for index := range newestFirst {
		items[len(newestFirst)-1-index] = newestFirst[index]
	}
	return messagePage{
		Items: items, Changes: []messageChange{}, NextCursor: next,
		HighWatermark: cursor.Snapshot, LatestSequence: latest, ResetRequired: reset,
	}, nil
}

func (repository *sqliteConversationRepository) messageChanges(ctx context.Context, conversationID string, after uint64, limit int) (messagePage, error) {
	minimum, latest, err := repository.journalBounds(ctx, "message", conversationID)
	if err != nil {
		return messagePage{}, err
	}
	if after > latest {
		return messagePage{}, errConversationInvalid
	}
	if minimum > 0 && after+1 < minimum {
		return repository.messageSnapshot(ctx, conversationID, "", limit, true)
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT sequence, deleted, payload FROM change_journal
		WHERE stream = 'message' AND scope = ? AND sequence > ? ORDER BY sequence LIMIT ?`, conversationID, after, maximumConversationChangePageSize+1)
	if err != nil {
		return messagePage{}, err
	}
	defer rows.Close()
	changes := make([]messageChange, 0, maximumConversationChangePageSize)
	for rows.Next() {
		var change messageChange
		var deleted int
		var payload []byte
		if err := rows.Scan(&change.Sequence, &deleted, &payload); err != nil || json.Unmarshal(payload, &change.Value) != nil {
			return messagePage{}, errors.New("invalid message journal")
		}
		change.Deleted = deleted != 0
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return messagePage{}, err
	}
	hasMore := len(changes) > maximumConversationChangePageSize
	if hasMore {
		changes = changes[:maximumConversationChangePageSize]
	}
	watermark := latest
	if hasMore && len(changes) > 0 {
		watermark = changes[len(changes)-1].Sequence
	}
	return messagePage{
		Items: []chatMessage{}, Changes: changes, HighWatermark: watermark,
		LatestSequence: latest, HasMoreChanges: hasMore,
	}, nil
}

func (repository *sqliteConversationRepository) MessagesForCompletion(ctx context.Context, id string) ([]chatMessage, conversationView, error) {
	view, err := repository.GetConversation(ctx, id)
	if err != nil {
		return nil, conversationView{}, err
	}
	rows, err := repository.db.QueryContext(ctx, `
		SELECT id, revision, seq, role, content, created_at, status FROM messages
		WHERE conversation_id = ? AND role IN ('user', 'assistant') AND status = 'complete'
		ORDER BY seq DESC LIMIT 100`, id)
	if err != nil {
		return nil, conversationView{}, err
	}
	defer rows.Close()
	newest := make([]chatMessage, 0, 100)
	for rows.Next() {
		value, err := scanMessage(rows)
		if err != nil {
			return nil, conversationView{}, err
		}
		newest = append(newest, value)
	}
	history := make([]chatMessage, len(newest))
	for index := range newest {
		history[len(newest)-1-index] = newest[index]
	}
	return history, view, rows.Err()
}

func (repository *sqliteConversationRepository) BeginTurn(ctx context.Context, conversationID, prompt string, now time.Time) (conversationTurn, error) {
	prompt, now = strings.TrimSpace(prompt), now.UTC()
	if uuid.Validate(conversationID) != nil || prompt == "" || !utf8.ValidString(prompt) || len(prompt) > 32<<10 || now.IsZero() {
		return conversationTurn{}, errConversationInvalid
	}
	history, before, err := repository.MessagesForCompletion(ctx, conversationID)
	if err != nil {
		return conversationTurn{}, err
	}
	if before.State == "running" {
		return conversationTurn{}, errConversationConflict
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return conversationTurn{}, err
	}
	defer tx.Rollback()
	var maximumSequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM messages WHERE conversation_id = ?`, conversationID).Scan(&maximumSequence); err != nil {
		return conversationTurn{}, err
	}
	user := chatMessage{ID: uuid.NewString(), Sequence: maximumSequence + 1, Role: "user", Content: prompt, CreatedAt: now, Status: "complete"}
	assistant := chatMessage{ID: uuid.NewString(), Sequence: maximumSequence + 2, Role: "assistant", Content: "", CreatedAt: now, Status: "streaming"}
	userSequence, err := appendJournal(ctx, tx, "message", conversationID, user.ID, false, user, now)
	if err != nil {
		return conversationTurn{}, err
	}
	user.Revision = userSequence
	if err := insertMessage(ctx, tx, conversationID, user); err != nil || updateJournalPayload(ctx, tx, userSequence, user) != nil {
		return conversationTurn{}, firstError(err, errors.New("update user journal"))
	}
	assistantSequence, err := appendJournal(ctx, tx, "message", conversationID, assistant.ID, false, assistant, now)
	if err != nil {
		return conversationTurn{}, err
	}
	assistant.Revision = assistantSequence
	if err := insertMessage(ctx, tx, conversationID, assistant); err != nil || updateJournalPayload(ctx, tx, assistantSequence, assistant) != nil {
		return conversationTurn{}, firstError(err, errors.New("update assistant journal"))
	}
	updated := before
	updated.State, updated.MessageCount, updated.UpdatedAt = "running", before.MessageCount+2, now
	conversationSequence, err := appendJournal(ctx, tx, "conversation", "", conversationID, false, updated, now)
	if err != nil {
		return conversationTurn{}, err
	}
	updated.Revision = conversationSequence
	result, err := tx.ExecContext(ctx, `UPDATE conversations SET revision=?, state='running', message_count=?, updated_at=?, active_assistant_id=? WHERE id=? AND revision=? AND state <> 'running'`,
		conversationSequence, updated.MessageCount, now.UnixNano(), assistant.ID, conversationID, before.Revision)
	if err != nil {
		return conversationTurn{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return conversationTurn{}, errConversationConflict
	}
	if err := updateJournalPayload(ctx, tx, conversationSequence, updated); err != nil {
		return conversationTurn{}, err
	}
	if err := repository.commitMutationMany(ctx, tx, [][2]string{{"message", conversationID}, {"conversation", ""}}); err != nil {
		return conversationTurn{}, err
	}
	return conversationTurn{
		History:      history,
		User:         messageChange{Sequence: userSequence, Value: user},
		Assistant:    messageChange{Sequence: assistantSequence, Value: assistant},
		Conversation: conversationChange{Sequence: conversationSequence, Value: updated},
	}, nil
}

func (repository *sqliteConversationRepository) AppendAssistantChunk(ctx context.Context, conversationID, assistantID, chunk string) (messageChange, error) {
	if uuid.Validate(conversationID) != nil || uuid.Validate(assistantID) != nil || chunk == "" || !utf8.ValidString(chunk) {
		return messageChange{}, errConversationInvalid
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return messageChange{}, err
	}
	defer tx.Rollback()
	value, err := messageByID(ctx, tx, conversationID, assistantID)
	if err != nil {
		return messageChange{}, err
	}
	if value.Status != "streaming" || value.Role != "assistant" || len(value.Content)+len(chunk) > maximumAssistantBytes {
		return messageChange{}, errConversationConflict
	}
	var active string
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT active_assistant_id, state FROM conversations WHERE id=?`, conversationID).Scan(&active, &state); err != nil || active != assistantID || state != "running" {
		return messageChange{}, errConversationConflict
	}
	value.Content += chunk
	sequence, err := appendJournal(ctx, tx, "message", conversationID, assistantID, false, value, time.Now().UTC())
	if err != nil {
		return messageChange{}, err
	}
	previousRevision := value.Revision
	value.Revision = sequence
	result, err := tx.ExecContext(ctx, `UPDATE messages SET revision=?, content=? WHERE id=? AND conversation_id=? AND revision=? AND status='streaming'`,
		sequence, value.Content, assistantID, conversationID, previousRevision)
	if err != nil {
		return messageChange{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return messageChange{}, errConversationConflict
	}
	if err := updateJournalPayload(ctx, tx, sequence, value); err != nil {
		return messageChange{}, err
	}
	if err := repository.commitMutation(ctx, tx, "message", conversationID); err != nil {
		return messageChange{}, err
	}
	return messageChange{Sequence: sequence, Value: value}, nil
}

func (repository *sqliteConversationRepository) FinishTurn(ctx context.Context, conversationID, assistantID, model string, now time.Time) (messageChange, conversationChange, error) {
	return repository.finishTurn(ctx, conversationID, assistantID, model, "complete", "idle", now)
}

func (repository *sqliteConversationRepository) AbortTurn(ctx context.Context, conversationID, assistantID, status string, now time.Time) (messageChange, conversationChange, error) {
	if status != "cancelled" && status != "failed" {
		return messageChange{}, conversationChange{}, errConversationInvalid
	}
	conversationState := "failed"
	if status == "cancelled" {
		conversationState = "idle"
	}
	return repository.finishTurn(ctx, conversationID, assistantID, "", status, conversationState, now)
}

func (repository *sqliteConversationRepository) finishTurn(ctx context.Context, conversationID, assistantID, model, messageStatus, conversationState string, now time.Time) (messageChange, conversationChange, error) {
	if uuid.Validate(conversationID) != nil || uuid.Validate(assistantID) != nil || len(model) > 120 || now.IsZero() {
		return messageChange{}, conversationChange{}, errConversationInvalid
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return messageChange{}, conversationChange{}, err
	}
	defer tx.Rollback()
	message, err := messageByID(ctx, tx, conversationID, assistantID)
	if err != nil {
		return messageChange{}, conversationChange{}, err
	}
	if message.Status != "streaming" || message.Role != "assistant" || messageStatus == "complete" && strings.TrimSpace(message.Content) == "" {
		return messageChange{}, conversationChange{}, errConversationConflict
	}
	var current conversationView
	var updated int64
	var active string
	err = tx.QueryRowContext(ctx, `SELECT id,title,model,revision,state,message_count,updated_at,active_assistant_id FROM conversations WHERE id=?`, conversationID).
		Scan(&current.ID, &current.Title, &current.Model, &current.Revision, &current.State, &current.MessageCount, &updated, &active)
	if err != nil || current.State != "running" || active != assistantID {
		return messageChange{}, conversationChange{}, errConversationConflict
	}
	current.UpdatedAt = time.Unix(0, updated).UTC()
	message.Status = messageStatus
	messageSequence, err := appendJournal(ctx, tx, "message", conversationID, assistantID, false, message, now)
	if err != nil {
		return messageChange{}, conversationChange{}, err
	}
	previousMessageRevision := message.Revision
	message.Revision = messageSequence
	result, err := tx.ExecContext(ctx, `UPDATE messages SET revision=?,status=? WHERE id=? AND conversation_id=? AND revision=? AND status='streaming'`,
		messageSequence, messageStatus, assistantID, conversationID, previousMessageRevision)
	if err != nil {
		return messageChange{}, conversationChange{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return messageChange{}, conversationChange{}, errConversationConflict
	}
	if err := updateJournalPayload(ctx, tx, messageSequence, message); err != nil {
		return messageChange{}, conversationChange{}, err
	}
	current.State, current.UpdatedAt = conversationState, now.UTC()
	if model != "" {
		current.Model = model
	}
	conversationSequence, err := appendJournal(ctx, tx, "conversation", "", conversationID, false, current, now)
	if err != nil {
		return messageChange{}, conversationChange{}, err
	}
	previousConversationRevision := current.Revision
	current.Revision = conversationSequence
	result, err = tx.ExecContext(ctx, `UPDATE conversations SET revision=?,state=?,model=?,updated_at=?,active_assistant_id='' WHERE id=? AND revision=? AND active_assistant_id=?`,
		conversationSequence, conversationState, current.Model, now.UTC().UnixNano(), conversationID, previousConversationRevision, assistantID)
	if err != nil {
		return messageChange{}, conversationChange{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return messageChange{}, conversationChange{}, errConversationConflict
	}
	if err := updateJournalPayload(ctx, tx, conversationSequence, current); err != nil {
		return messageChange{}, conversationChange{}, err
	}
	if err := repository.commitMutationMany(ctx, tx, [][2]string{{"message", conversationID}, {"conversation", ""}}); err != nil {
		return messageChange{}, conversationChange{}, err
	}
	return messageChange{Sequence: messageSequence, Value: message}, conversationChange{Sequence: conversationSequence, Value: current}, nil
}

func insertMessage(ctx context.Context, tx *sql.Tx, conversationID string, value chatMessage) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,seq,revision,role,content,status,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		value.ID, conversationID, value.Sequence, value.Revision, value.Role, value.Content, value.Status, value.CreatedAt.UTC().UnixNano())
	return err
}

type rowScanner interface{ Scan(...any) error }

func scanMessage(row rowScanner) (chatMessage, error) {
	var value chatMessage
	var created int64
	err := row.Scan(&value.ID, &value.Revision, &value.Sequence, &value.Role, &value.Content, &created, &value.Status)
	value.CreatedAt = time.Unix(0, created).UTC()
	return value, err
}

func messageByID(ctx context.Context, tx *sql.Tx, conversationID, id string) (chatMessage, error) {
	value, err := scanMessage(tx.QueryRowContext(ctx, `
		SELECT id,revision,seq,role,content,created_at,status FROM messages WHERE conversation_id=? AND id=?`, conversationID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return chatMessage{}, errConversationNotFound
	}
	return value, err
}

func appendJournal(ctx context.Context, tx *sql.Tx, stream, scope, recordID string, deleted bool, value any, occurredAt time.Time) (uint64, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO change_journal(stream,scope,record_id,revision,deleted,payload,created_at) VALUES(?,?,?,0,?,?,?)`,
		stream, scope, recordID, boolInt(deleted), payload, occurredAt.UTC().UnixNano())
	if err != nil {
		return 0, err
	}
	sequence, err := result.LastInsertId()
	if err != nil || sequence <= 0 {
		return 0, errors.New("allocate conversation change sequence")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE change_journal SET revision=? WHERE sequence=?`, sequence, sequence); err != nil {
		return 0, err
	}
	return uint64(sequence), nil
}

func updateJournalPayload(ctx context.Context, tx *sql.Tx, sequence uint64, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE change_journal SET payload=? WHERE sequence=?`, payload, sequence)
	return err
}

func (repository *sqliteConversationRepository) commitMutation(ctx context.Context, tx *sql.Tx, stream, scope string) error {
	return repository.commitMutationMany(ctx, tx, [][2]string{{stream, scope}})
}

func (repository *sqliteConversationRepository) commitMutationMany(ctx context.Context, tx *sql.Tx, streams [][2]string) error {
	for _, stream := range streams {
		if _, err := tx.ExecContext(ctx, `DELETE FROM change_journal WHERE stream=? AND scope=? AND sequence < COALESCE((
			SELECT sequence FROM change_journal WHERE stream=? AND scope=? ORDER BY sequence DESC LIMIT 1 OFFSET ?
		), 0)`, stream[0], stream[1], stream[0], stream[1], repository.journalRetention-1); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return secureConversationDatabaseFiles(repository.path)
}

func (repository *sqliteConversationRepository) journalHighWatermark(ctx context.Context, stream, scope string) (uint64, error) {
	var value uint64
	err := repository.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM change_journal WHERE stream=? AND scope=?`, stream, scope).Scan(&value)
	return value, err
}

func (repository *sqliteConversationRepository) journalBounds(ctx context.Context, stream, scope string) (uint64, uint64, error) {
	var minimum, maximum uint64
	err := repository.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(sequence),0),COALESCE(MAX(sequence),0) FROM change_journal WHERE stream=? AND scope=?`, stream, scope).Scan(&minimum, &maximum)
	return minimum, maximum, err
}

func (repository *sqliteConversationRepository) encodeCursor(value conversationCursor) (string, error) {
	if value.Version != 1 || value.Kind == "" || value.Snapshot == 0 && (value.UpdatedAt != 0 || value.BeforeSeq != 0) {
		return "", errConversationInvalid
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, repository.cursorKey)
	_, _ = mac.Write(payload)
	signature := mac.Sum(nil)[:16]
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (repository *sqliteConversationRepository) decodeCursor(raw, kind, scope string) (conversationCursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || len(raw) > 512 {
		return conversationCursor{}, errConversationInvalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		return conversationCursor{}, errConversationInvalid
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(signature) != 16 || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return conversationCursor{}, errConversationInvalid
	}
	mac := hmac.New(sha256.New, repository.cursorKey)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)[:16]) {
		return conversationCursor{}, errConversationInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var value conversationCursor
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) == nil || value.Version != 1 || value.Kind != kind || value.Scope != scope || value.Snapshot == 0 {
		return conversationCursor{}, errConversationInvalid
	}
	return value, nil
}

func normalizedConversationState(value string) string {
	switch value {
	case "running", "failed":
		return value
	default:
		return "idle"
	}
}

func normalizedMessageStatus(value string) string {
	switch value {
	case "streaming", "failed", "cancelled":
		return value
	default:
		return "complete"
	}
}

func validMessageRole(value string) bool {
	return value == "user" || value == "assistant" || value == "system" || value == "tool"
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func syncConversationDatabaseFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal"} {
		file, err := os.OpenFile(candidate, os.O_RDONLY, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
