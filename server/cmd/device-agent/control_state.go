package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

const (
	controlStateVersion             = 1
	maximumControlStateBytes        = 16 << 20
	maximumControlPlaintext         = 12 << 20
	controlStateFileExtension       = ".remote-control.enc"
	maximumPersistedTaskLogsPerTask = 2000
	maximumPersistedTaskLogsTotal   = 10000
	maximumPersistedProjectChanges  = 5000
	maximumPersistedTaskChanges     = 5000
)

type controlAuthState struct {
	RefreshToken     string    `json:"refreshToken,omitempty"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt,omitempty"`
	SessionID        uuid.UUID `json:"sessionId,omitempty"`
	Scope            string    `json:"scope,omitempty"`
}

type controlSyncState struct {
	HighWatermark   uint64                          `json:"highWatermark"`
	Reset           bool                            `json:"reset"`
	Projects        map[string]localProject         `json:"projects"`
	TaskProjections map[string]taskProjectionCursor `json:"taskProjections,omitempty"`
	Pending         []remotecontrol.DeviceChange    `json:"pending"`
}

type taskProjectionCursor struct {
	Revision uint64 `json:"revision"`
	Present  bool   `json:"present"`
}

type localProject struct {
	ID               uuid.UUID `json:"id"`
	DisplayName      string    `json:"displayName"`
	RelativePath     string    `json:"relativePath"`
	Fingerprint      string    `json:"fingerprint"`
	Revision         uint64    `json:"revision"`
	RegistryRevision uint64    `json:"registryRevision,omitempty"`
	ObservedAt       time.Time `json:"observedAt,omitempty"`
	State            string    `json:"state"`
}

type localProjectChange struct {
	Sequence uint64       `json:"sequence"`
	Deleted  bool         `json:"deleted,omitempty"`
	Project  localProject `json:"project"`
}

type localCommand struct {
	Command            remotecontrol.Command   `json:"command"`
	ExecutionState     string                  `json:"executionState"`
	DesiredAck         string                  `json:"desiredAck"`
	AckSent            bool                    `json:"ackSent"`
	FailureCode        string                  `json:"failureCode,omitempty"`
	RequiredChange     uint64                  `json:"requiredChange,omitempty"`
	RequiredEvent      uint64                  `json:"requiredEvent,omitempty"`
	DecodedTask        *localTaskSpec          `json:"decodedTask,omitempty"`
	ProjectSync        *projectSyncCommandBody `json:"projectSync,omitempty"`
	CancellationTaskID *uuid.UUID              `json:"cancellationTaskId,omitempty"`
}

type localTaskSpec struct {
	TaskID           uuid.UUID       `json:"taskId"`
	ProjectID        *uuid.UUID      `json:"projectId,omitempty"`
	TaskType         string          `json:"taskType"`
	Title            string          `json:"title"`
	ExpectedRevision *uint64         `json:"expectedRevision,omitempty"`
	Input            json.RawMessage `json:"input"`
}

type localTask struct {
	Spec            localTaskSpec `json:"spec"`
	CommandID       uuid.UUID     `json:"commandId"`
	PeerManaged     bool          `json:"peerManaged,omitempty"`
	Status          string        `json:"status"`
	Revision        uint64        `json:"revision"`
	ChangeSequence  uint64        `json:"changeSequence"`
	CreatedAt       time.Time     `json:"createdAt"`
	NextLogSequence uint64        `json:"nextLogSequence"`
	CancelRequested bool          `json:"cancelRequested"`
	StartedAt       *time.Time    `json:"startedAt,omitempty"`
	FinishedAt      *time.Time    `json:"finishedAt,omitempty"`
	TerminalResult  string        `json:"terminalResult,omitempty"`
}

type localTaskChange struct {
	Sequence uint64    `json:"sequence"`
	Deleted  bool      `json:"deleted,omitempty"`
	Task     localTask `json:"task"`
}

type controlPersistentState struct {
	Version                         int                                `json:"version"`
	DeviceID                        uuid.UUID                          `json:"deviceId"`
	Auth                            controlAuthState                   `json:"auth"`
	Sync                            controlSyncState                   `json:"sync"`
	ProjectChanges                  []localProjectChange               `json:"projectChanges"`
	ProjectHighWatermark            uint64                             `json:"projectHighWatermark"`
	ProjectMinimumAvailableSequence uint64                             `json:"projectMinimumAvailableSequence"`
	Commands                        map[string]localCommand            `json:"commands"`
	Tasks                           map[string]localTask               `json:"tasks"`
	TaskChanges                     []localTaskChange                  `json:"taskChanges"`
	TaskLogs                        map[string][]remotecontrol.TaskLog `json:"taskLogs"`
	TaskHighWatermark               uint64                             `json:"taskHighWatermark"`
	TaskMinimumAvailableSequence    uint64                             `json:"taskMinimumAvailableSequence"`
	CancelledTasks                  map[string]bool                    `json:"cancelledTasks"`
	PendingEvents                   []remotecontrol.DeviceEventInput   `json:"pendingEvents"`
	EventAckedSequence              uint64                             `json:"eventAckedSequence"`
	NextEventSequence               uint64                             `json:"nextEventSequence"`
}

type controlStateEnvelope struct {
	Version    int       `json:"version"`
	DeviceID   uuid.UUID `json:"deviceId"`
	Nonce      string    `json:"nonce"`
	Ciphertext string    `json:"ciphertext"`
}

type controlStateStore struct {
	mu       sync.Mutex
	path     string
	deviceID uuid.UUID
	aead     cipher.AEAD
	state    controlPersistentState
}

func loadControlState(state *agentState) (*controlStateStore, error) {
	if state == nil || state.DeviceID == uuid.Nil || len(state.identity) == 0 || state.path == "" {
		return nil, errors.New("control state identity is invalid")
	}
	keyMaterial := append([]byte("wenzwork-device-control-state:v1\x00"+state.DeviceID.String()+"\x00"), state.identity...)
	key := sha256.Sum256(keyMaterial)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	store := &controlStateStore{
		path: state.path + controlStateFileExtension, deviceID: state.DeviceID, aead: aead,
		state: newControlPersistentState(state.DeviceID),
	}
	contents, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read encrypted control state: %w", err)
	}
	if len(contents) == 0 || len(contents) > maximumControlStateBytes {
		return nil, errors.New("encrypted control state size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var envelope controlStateEnvelope
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || envelope.Version != controlStateVersion || envelope.DeviceID != state.DeviceID {
		return nil, errors.New("encrypted control state envelope is invalid")
	}
	nonce, err := decodeCanonicalBase64(envelope.Nonce, aead.NonceSize(), 64)
	if err != nil {
		return nil, errors.New("encrypted control state nonce is invalid")
	}
	ciphertext, err := decodeCanonicalBase64(envelope.Ciphertext, -1, maximumControlStateBytes)
	if err != nil || len(ciphertext) < aead.Overhead() {
		return nil, errors.New("encrypted control state ciphertext is invalid")
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, controlStateAAD(state.DeviceID))
	if err != nil || len(plaintext) == 0 || len(plaintext) > maximumControlPlaintext {
		return nil, errors.New("encrypted control state authentication failed")
	}
	decoder = json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	var persisted controlPersistentState
	if decoder.Decode(&persisted) != nil || decoder.Decode(new(any)) != io.EOF || validateControlPersistentState(&persisted, state.DeviceID) != nil {
		return nil, errors.New("encrypted control state payload is invalid")
	}
	store.state = persisted
	return store, nil
}

func newControlPersistentState(deviceID uuid.UUID) controlPersistentState {
	return controlPersistentState{
		Version: controlStateVersion, DeviceID: deviceID,
		Sync: controlSyncState{Projects: map[string]localProject{}, TaskProjections: map[string]taskProjectionCursor{}, Pending: []remotecontrol.DeviceChange{}}, ProjectChanges: []localProjectChange{},
		Commands: map[string]localCommand{}, Tasks: map[string]localTask{}, TaskChanges: []localTaskChange{}, TaskLogs: map[string][]remotecontrol.TaskLog{}, CancelledTasks: map[string]bool{},
		PendingEvents: []remotecontrol.DeviceEventInput{}, NextEventSequence: 1,
	}
}

func validateControlPersistentState(state *controlPersistentState, deviceID uuid.UUID) error {
	if state == nil || state.Version != controlStateVersion || state.DeviceID != deviceID || state.NextEventSequence == 0 {
		return errors.New("invalid control state")
	}
	if state.Sync.Projects == nil {
		state.Sync.Projects = map[string]localProject{}
	}
	if state.Sync.Pending == nil {
		state.Sync.Pending = []remotecontrol.DeviceChange{}
	}
	if state.Sync.TaskProjections == nil {
		state.Sync.TaskProjections = map[string]taskProjectionCursor{}
	}
	if state.ProjectChanges == nil {
		state.ProjectChanges = []localProjectChange{}
	}
	if state.ProjectHighWatermark == 0 && len(state.Sync.Projects) > 0 {
		state.ProjectHighWatermark = 1
	}
	if state.ProjectMinimumAvailableSequence == 0 {
		state.ProjectMinimumAvailableSequence = state.ProjectHighWatermark + 1
		if len(state.ProjectChanges) > 0 {
			state.ProjectMinimumAvailableSequence = state.ProjectChanges[0].Sequence
		}
	}
	if state.Commands == nil {
		state.Commands = map[string]localCommand{}
	}
	if state.Tasks == nil {
		state.Tasks = map[string]localTask{}
	}
	if state.TaskChanges == nil {
		state.TaskChanges = []localTaskChange{}
	}
	if state.TaskLogs == nil {
		state.TaskLogs = map[string][]remotecontrol.TaskLog{}
	}
	for key, task := range state.Tasks {
		if task.CreatedAt.IsZero() && task.Revision > 0 && task.Revision <= uint64(^uint64(0)>>1) {
			task.CreatedAt = time.UnixMilli(int64(task.Revision)).UTC()
		}
		if task.ChangeSequence == 0 {
			task.ChangeSequence = 1
		}
		state.TaskHighWatermark = max(state.TaskHighWatermark, task.ChangeSequence)
		state.Tasks[key] = task
	}
	if state.TaskMinimumAvailableSequence == 0 {
		state.TaskMinimumAvailableSequence = state.TaskHighWatermark + 1
		if len(state.TaskChanges) > 0 {
			state.TaskMinimumAvailableSequence = state.TaskChanges[0].Sequence
		}
	}
	if state.CancelledTasks == nil {
		state.CancelledTasks = map[string]bool{}
	}
	if state.PendingEvents == nil {
		state.PendingEvents = []remotecontrol.DeviceEventInput{}
	}
	if len(state.PendingEvents) > 0 {
		metadataOnly := make([]remotecontrol.DeviceEventInput, 0, len(state.PendingEvents))
		for _, event := range state.PendingEvents {
			if event.Type == "task.log" || event.Log != nil {
				continue
			}
			metadataOnly = append(metadataOnly, event)
		}
		state.PendingEvents = metadataOnly
	}
	if len(state.Sync.Pending) > 10000 || len(state.Sync.TaskProjections) > 10000 || len(state.ProjectChanges) > maximumPersistedProjectChanges || len(state.TaskChanges) > maximumPersistedTaskChanges || len(state.PendingEvents) > 10000 || len(state.Commands) > 10000 || len(state.Tasks) > 10000 || len(state.TaskLogs) > 10000 {
		return errors.New("control state collection is too large")
	}
	for taskID, cursor := range state.Sync.TaskProjections {
		parsed, err := uuid.Parse(taskID)
		if err != nil || parsed == uuid.Nil || cursor.Revision == 0 {
			return errors.New("invalid task projection cursor")
		}
	}
	if err := validateProjectChangeJournal(state); err != nil {
		return err
	}
	if err := validateTaskChangeJournal(state); err != nil {
		return err
	}
	logCount := 0
	for taskID, logs := range state.TaskLogs {
		if _, exists := state.Tasks[taskID]; !exists || len(logs) > maximumPersistedTaskLogsPerTask {
			return errors.New("invalid persisted task logs")
		}
		logCount += len(logs)
		var priorSequence uint64
		for _, entry := range logs {
			if entry.Sequence == 0 || entry.OccurredAt.IsZero() || !validTaskLogStream(entry.Stream) || len([]byte(entry.Content)) > remotecontrol.MaximumLogContentBytes {
				return errors.New("invalid persisted task log")
			}
			if entry.Sequence <= priorSequence {
				return errors.New("persisted task logs are out of order")
			}
			priorSequence = entry.Sequence
		}
	}
	if logCount > maximumPersistedTaskLogsTotal {
		return errors.New("too many persisted task logs")
	}
	return nil
}

func validateProjectChangeJournal(state *controlPersistentState) error {
	prior := uint64(0)
	for _, change := range state.ProjectChanges {
		if change.Sequence == 0 || change.Sequence <= prior || change.Sequence > state.ProjectHighWatermark || change.Project.ID == uuid.Nil {
			return errors.New("invalid project change journal")
		}
		prior = change.Sequence
	}
	if len(state.ProjectChanges) > 0 && state.ProjectMinimumAvailableSequence != state.ProjectChanges[0].Sequence {
		return errors.New("invalid project journal retention boundary")
	}
	return nil
}

func validateTaskChangeJournal(state *controlPersistentState) error {
	prior := uint64(0)
	for _, change := range state.TaskChanges {
		if change.Sequence == 0 || change.Sequence <= prior || change.Sequence > state.TaskHighWatermark || change.Task.Spec.TaskID == uuid.Nil {
			return errors.New("invalid task change journal")
		}
		prior = change.Sequence
	}
	if len(state.TaskChanges) > 0 && state.TaskMinimumAvailableSequence != state.TaskChanges[0].Sequence {
		return errors.New("invalid task journal retention boundary")
	}
	return nil
}

func validTaskLogStream(value string) bool {
	switch value {
	case "stdout", "stderr", "system", "tool":
		return true
	default:
		return false
	}
}

func (store *controlStateStore) snapshot() (controlPersistentState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneControlPersistentState(store.state)
}

func (store *controlStateStore) update(mutate func(*controlPersistentState) error) error {
	if store == nil || mutate == nil {
		return errors.New("control state update is invalid")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	candidate, err := cloneControlPersistentState(store.state)
	if err != nil {
		return err
	}
	if err := mutate(&candidate); err != nil {
		return err
	}
	if err := validateControlPersistentState(&candidate, store.deviceID); err != nil {
		return err
	}
	if err := store.write(candidate); err != nil {
		return err
	}
	store.state = candidate
	return nil
}

func cloneControlPersistentState(state controlPersistentState) (controlPersistentState, error) {
	payload, err := json.Marshal(state)
	if err != nil {
		return controlPersistentState{}, err
	}
	var cloned controlPersistentState
	if json.Unmarshal(payload, &cloned) != nil {
		return controlPersistentState{}, errors.New("clone control state failed")
	}
	return cloned, nil
}

func (store *controlStateStore) write(state controlPersistentState) error {
	plaintext, err := json.Marshal(state)
	if err != nil || len(plaintext) > maximumControlPlaintext {
		return errors.New("control state payload is too large")
	}
	nonce := make([]byte, store.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ciphertext := store.aead.Seal(nil, nonce, plaintext, controlStateAAD(store.deviceID))
	envelope := controlStateEnvelope{
		Version: controlStateVersion, DeviceID: store.deviceID,
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}
	contents, err := json.Marshal(envelope)
	if err != nil || len(contents) > maximumControlStateBytes {
		return errors.New("encrypted control state is too large")
	}
	contents = append(contents, '\n')
	parent := filepath.Dir(store.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".device-control-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) error {
		_ = temporary.Close()
		return cause
	}
	if err := temporary.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fail(err)
	}
	if err := temporary.Sync(); err != nil {
		return fail(err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return err
	}
	return os.Chmod(store.path, 0o600)
}

func controlStateAAD(deviceID uuid.UUID) []byte {
	return []byte("wenzwork-device-control-state-envelope:v1\x00" + deviceID.String())
}

func decodeCanonicalBase64(value string, exactSize, maximum int) ([]byte, error) {
	if value == "" || len(value) > maximum*2 {
		return nil, errors.New("base64 value is invalid")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || exactSize >= 0 && len(decoded) != exactSize || len(decoded) > maximum || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("base64 value is invalid")
	}
	return decoded, nil
}
