package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	taskPayloadChunkBytes = 32 << 10
	taskPayloadQuotaBytes = 8 << 20
	maximumTaskPayloads   = 8
	taskPayloadTTL        = 15 * time.Minute
)

type taskPayloadTransfer struct {
	TransferID     string    `json:"transferId"`
	ProjectID      string    `json:"projectId"`
	TargetMethod   string    `json:"targetMethod"`
	TotalBytes     int64     `json:"totalBytes"`
	SHA256         string    `json:"sha256"`
	AcceptedOffset int64     `json:"acceptedOffset"`
	ExpiresAt      time.Time `json:"expiresAt"`
	Committed      bool      `json:"committed"`
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
}

type taskPayloadStore struct {
	mu               sync.Mutex
	root             string
	now              func() time.Time
	quotaBytes       int64
	maximumTransfers int
	transfers        map[string]*taskPayloadTransfer
}

func taskPayloadStoreFor(state *agentState) (*taskPayloadStore, error) {
	if state == nil || strings.TrimSpace(state.path) == "" {
		return nil, errRPCCapability
	}
	state.servicesMu.Lock()
	defer state.servicesMu.Unlock()
	if state.taskPayloads != nil {
		return state.taskPayloads, nil
	}
	store, err := openTaskPayloadStore(filepath.Join(filepath.Dir(state.path), "task-payloads"), time.Now)
	if err != nil {
		return nil, err
	}
	state.taskPayloads = store
	return store, nil
}

func openTaskPayloadStore(root string, now func() time.Time) (*taskPayloadStore, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	store := &taskPayloadStore{
		root: root, now: now, quotaBytes: taskPayloadQuotaBytes,
		maximumTransfers: maximumTaskPayloads, transfers: map[string]*taskPayloadTransfer{},
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		metadataPath := filepath.Join(root, entry.Name())
		contents, readErr := os.ReadFile(metadataPath)
		var transfer taskPayloadTransfer
		if readErr != nil || json.Unmarshal(contents, &transfer) != nil ||
			!store.validLoadedTransfer(entry.Name(), &transfer) || len(store.transfers) >= store.maximumTransfers ||
			store.reservedBytesLocked()+transfer.TotalBytes > store.quotaBytes {
			store.removeLoadedArtifacts(entry.Name(), transfer.TransferID)
			continue
		}
		store.transfers[transfer.TransferID] = &transfer
	}
	store.cleanupExpiredLocked()
	store.cleanupOrphanedLocked()
	return store, nil
}

func (d dispatcher) callTaskPayloadRPC(method string, input rpcInput) (any, uint64, error) {
	store, err := taskPayloadStoreFor(d.state)
	if err != nil {
		return nil, 0, err
	}
	projectID := strings.TrimSpace(d.requestProjectID)
	if uuid.Validate(projectID) != nil {
		return nil, 0, errRPCProject
	}
	var output any
	switch method {
	case "task.payload.prepare":
		output, err = store.prepare(projectID, input)
	case "task.payload.chunk":
		output, err = store.chunk(projectID, input)
	case "task.payload.commit":
		output, err = store.commit(projectID, input)
	case "task.payload.abort":
		output, err = store.abort(projectID, input)
	default:
		err = errRPCNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return output, d.state.revisionValue(), nil
}

func (d dispatcher) resolveTaskPayloadInput(method string, input rpcInput) (rpcInput, error) {
	value, present := input["payloadTransferId"]
	if !present {
		return input, nil
	}
	if len(input) != 1 {
		return nil, errRPCInvalid
	}
	transferID, ok := value.(string)
	if !ok || uuid.Validate(transferID) != nil {
		return nil, errRPCInvalid
	}
	store, err := taskPayloadStoreFor(d.state)
	if err != nil {
		return nil, err
	}
	return store.resolve(strings.TrimSpace(d.requestProjectID), method, transferID)
}

func (store *taskPayloadStore) prepare(projectID string, input rpcInput) (any, error) {
	if !onlyTaskPayloadFields(input, "transferId", "targetMethod", "totalBytes", "sha256") {
		return nil, errRPCInvalid
	}
	transferID, ok := optionalInputString(input, "transferId", 64)
	if !ok {
		return nil, errRPCInvalid
	}
	if transferID == "" {
		transferID = uuid.NewString()
	}
	targetMethod, methodOK := optionalInputString(input, "targetMethod", 80)
	total, present, totalOK := optionalUint64(input, "totalBytes")
	digest, digestOK := optionalInputString(input, "sha256", 64)
	if uuid.Validate(transferID) != nil || !methodOK || !validTaskPayloadTarget(targetMethod) || !present || !totalOK || !digestOK ||
		total <= maximumRPCPayload || total > maximumTaskDefinitionBytes || !validSHA256Hex(digest) {
		return nil, errRPCInvalid
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	store.cleanupExpiredLocked()
	if existing := store.transfers[transferID]; existing != nil {
		if existing.ProjectID != projectID || existing.TargetMethod != targetMethod || existing.TotalBytes != int64(total) || existing.SHA256 != digest {
			return nil, errRPCIdempotency
		}
		return store.manifest(existing), nil
	}
	if len(store.transfers) >= store.maximumTransfers || store.reservedBytesLocked()+int64(total) > store.quotaBytes {
		return nil, errRPCBusy
	}
	transfer := &taskPayloadTransfer{
		TransferID: transferID, ProjectID: projectID, TargetMethod: targetMethod,
		TotalBytes: int64(total), SHA256: digest, ExpiresAt: store.now().UTC().Add(taskPayloadTTL),
	}
	file, err := os.OpenFile(store.partPath(transferID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(store.partPath(transferID))
		return nil, closeErr
	}
	store.transfers[transferID] = transfer
	if err := store.persistLocked(transfer); err != nil {
		delete(store.transfers, transferID)
		_ = os.Remove(store.partPath(transferID))
		return nil, err
	}
	return store.manifest(transfer), nil
}

func (store *taskPayloadStore) chunk(projectID string, input rpcInput) (any, error) {
	if !onlyTaskPayloadFields(input, "transferId", "offset", "base64Data", "chunkSha256") {
		return nil, errRPCInvalid
	}
	transferID, idOK := optionalInputString(input, "transferId", 64)
	offset, present, offsetOK := optionalUint64(input, "offset")
	encoded, dataOK := optionalInputString(input, "base64Data", 48<<10)
	digest, digestOK := optionalInputString(input, "chunkSha256", 64)
	data, decodeErr := decodeTaskPayloadBase64(encoded)
	if !idOK || uuid.Validate(transferID) != nil || !present || !offsetOK || !dataOK || !digestOK || decodeErr != nil ||
		len(data) == 0 || len(data) > taskPayloadChunkBytes || !validSHA256Hex(digest) || sha256Hex(data) != digest {
		return nil, errRPCInvalid
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	store.cleanupExpiredLocked()
	transfer := store.transfers[transferID]
	if transfer == nil {
		return nil, errRPCNotFound
	}
	if transfer.ProjectID != projectID {
		return nil, errRPCProject
	}
	if transfer.Committed || int64(offset)+int64(len(data)) > transfer.TotalBytes {
		return nil, errRPCInvalid
	}
	if int64(offset) < transfer.AcceptedOffset {
		existing := make([]byte, len(data))
		file, err := os.Open(store.partPath(transferID))
		if err != nil {
			return nil, err
		}
		_, readErr := file.ReadAt(existing, int64(offset))
		_ = file.Close()
		if (readErr != nil && !errors.Is(readErr, io.EOF)) || sha256Hex(existing) != digest {
			return nil, errRPCIdempotency
		}
		return map[string]any{"transferId": transferID, "nextOffset": transfer.AcceptedOffset, "replayed": true}, nil
	}
	if int64(offset) != transfer.AcceptedOffset {
		return nil, errRPCRevision
	}
	file, err := os.OpenFile(store.partPath(transferID), os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.WriteAt(data, int64(offset))
	closeErr := file.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	previousOffset := transfer.AcceptedOffset
	transfer.AcceptedOffset += int64(len(data))
	if err := store.persistLocked(transfer); err != nil {
		transfer.AcceptedOffset = previousOffset
		return nil, errors.Join(err, os.Truncate(store.partPath(transferID), previousOffset))
	}
	return map[string]any{"transferId": transferID, "nextOffset": transfer.AcceptedOffset, "replayed": false}, nil
}

func (store *taskPayloadStore) commit(projectID string, input rpcInput) (any, error) {
	if !onlyTaskPayloadFields(input, "transferId", "idempotencyKey") {
		return nil, errRPCInvalid
	}
	transferID, idOK := optionalInputString(input, "transferId", 64)
	idempotencyKey, keyOK := optionalInputString(input, "idempotencyKey", 128)
	if !idOK || uuid.Validate(transferID) != nil || !keyOK || strings.TrimSpace(idempotencyKey) == "" {
		return nil, errRPCInvalid
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	store.cleanupExpiredLocked()
	transfer := store.transfers[transferID]
	if transfer == nil {
		return nil, errRPCNotFound
	}
	if transfer.ProjectID != projectID {
		return nil, errRPCProject
	}
	if transfer.Committed {
		if transfer.IdempotencyKey != idempotencyKey {
			return nil, errRPCIdempotency
		}
		return store.commitResult(transfer, true), nil
	}
	if transfer.AcceptedOffset != transfer.TotalBytes {
		return nil, errRPCRevision
	}
	digest, err := sha256File(store.partPath(transferID))
	if err != nil {
		return nil, err
	}
	if digest != transfer.SHA256 {
		return nil, errRPCInvalid
	}
	if err := os.Rename(store.partPath(transferID), store.readyPath(transferID)); err != nil {
		return nil, err
	}
	transfer.Committed = true
	transfer.IdempotencyKey = idempotencyKey
	if err := store.persistLocked(transfer); err != nil {
		transfer.Committed = false
		transfer.IdempotencyKey = ""
		if rollbackErr := os.Rename(store.readyPath(transferID), store.partPath(transferID)); rollbackErr != nil {
			store.removeLocked(transferID)
			return nil, errors.Join(err, rollbackErr)
		}
		return nil, err
	}
	return store.commitResult(transfer, false), nil
}

func (store *taskPayloadStore) abort(projectID string, input rpcInput) (any, error) {
	if !onlyTaskPayloadFields(input, "transferId") {
		return nil, errRPCInvalid
	}
	transferID, ok := optionalInputString(input, "transferId", 64)
	if !ok || uuid.Validate(transferID) != nil {
		return nil, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	transfer := store.transfers[transferID]
	if transfer == nil {
		return map[string]any{"transferId": transferID, "aborted": true, "replayed": true}, nil
	}
	if transfer.ProjectID != projectID {
		return nil, errRPCProject
	}
	store.removeLocked(transferID)
	return map[string]any{"transferId": transferID, "aborted": true, "replayed": false}, nil
}

func (store *taskPayloadStore) resolve(projectID, method, transferID string) (rpcInput, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.cleanupExpiredLocked()
	transfer := store.transfers[transferID]
	if transfer == nil || !transfer.Committed {
		return nil, errRPCNotFound
	}
	if transfer.ProjectID != projectID {
		return nil, errRPCProject
	}
	if transfer.TargetMethod != method {
		return nil, errRPCInvalid
	}
	contents, err := os.ReadFile(store.readyPath(transferID))
	if err != nil || int64(len(contents)) != transfer.TotalBytes || sha256Hex(contents) != transfer.SHA256 {
		store.removeLocked(transferID)
		return nil, errRPCInvalid
	}
	var input rpcInput
	if err := json.Unmarshal(contents, &input); err != nil || input == nil {
		store.removeLocked(transferID)
		return nil, errRPCInvalid
	}
	return input, nil
}

// compactTaskPayloadResult removes only fields that the caller already sent in
// the committed, hash-verified payload. The Peer restores these fields from its
// local request before exposing the result to higher-level task models. This
// keeps taskPayload.v2 responses below the shared 56 KiB JSON ceiling without
// weakening that ceiling or silently truncating device-owned metadata.
func compactTaskPayloadResult(method string, output any, transferID string) (map[string]any, bool) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, false
	}
	var result map[string]any
	if json.Unmarshal(encoded, &result) != nil || result == nil {
		return nil, false
	}
	restore := map[string]string{}
	var paths map[string]string
	switch method {
	case "task.create", "task.update":
		paths = map[string]string{
			"definition.config":             "definition.config",
			"definition.plan":               "definition.plan",
			"definition.environment":        "definition.environment",
			"definition.acceptanceFeedback": "definition.acceptanceFeedback",
		}
	case "workflow.validate":
		paths = map[string]string{
			"definition.config":             "definition.config",
			"definition.plan":               "definition.plan",
			"definition.environment":        "definition.environment",
			"definition.acceptanceFeedback": "definition.acceptanceFeedback",
			"revision.nodes":                "revision.nodes",
			"revision.edges":                "revision.edges",
		}
	case "workflow.create", "workflow.revision.publish":
		paths = map[string]string{
			"task.definition.config":             "definition.config",
			"task.definition.plan":               "definition.plan",
			"task.definition.environment":        "definition.environment",
			"task.definition.acceptanceFeedback": "definition.acceptanceFeedback",
			"revision.nodes":                     "revision.nodes",
			"revision.edges":                     "revision.edges",
		}
	default:
		return nil, false
	}
	for responsePath, inputPath := range paths {
		if deleteTaskPayloadResultPath(result, strings.Split(responsePath, ".")) {
			restore[responsePath] = inputPath
		}
	}
	if len(restore) == 0 {
		return nil, false
	}
	result["taskPayloadResult"] = map[string]any{
		"version": 1, "transferId": transferID, "restore": restore,
	}
	return result, true
}

func deleteTaskPayloadResultPath(value map[string]any, path []string) bool {
	if len(path) == 0 {
		return false
	}
	if len(path) == 1 {
		if _, ok := value[path[0]]; !ok {
			return false
		}
		delete(value, path[0])
		return true
	}
	next, ok := value[path[0]].(map[string]any)
	return ok && deleteTaskPayloadResultPath(next, path[1:])
}

func (store *taskPayloadStore) manifest(transfer *taskPayloadTransfer) map[string]any {
	return map[string]any{
		"transferId": transfer.TransferID, "targetMethod": transfer.TargetMethod, "totalBytes": transfer.TotalBytes,
		"sha256": transfer.SHA256, "chunkBytes": taskPayloadChunkBytes, "acceptedOffset": transfer.AcceptedOffset,
		"expiresAt": transfer.ExpiresAt, "committed": transfer.Committed,
	}
}

func (store *taskPayloadStore) commitResult(transfer *taskPayloadTransfer, replayed bool) map[string]any {
	return map[string]any{
		"payloadTransferId": transfer.TransferID, "targetMethod": transfer.TargetMethod,
		"totalBytes": transfer.TotalBytes, "sha256": transfer.SHA256, "replayed": replayed,
	}
}

func (store *taskPayloadStore) persistLocked(transfer *taskPayloadTransfer) error {
	encoded, err := json.Marshal(transfer)
	if err != nil {
		return err
	}
	temporary := store.metaPath(transfer.TransferID) + ".tmp"
	defer os.Remove(temporary)
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, store.metaPath(transfer.TransferID))
}

func (store *taskPayloadStore) cleanupExpiredLocked() {
	now := store.now().UTC()
	for id, transfer := range store.transfers {
		if !now.Before(transfer.ExpiresAt) {
			store.removeLocked(id)
		}
	}
}

func (store *taskPayloadStore) removeLocked(id string) {
	delete(store.transfers, id)
	_ = os.Remove(store.metaPath(id))
	_ = os.Remove(store.metaPath(id) + ".tmp")
	_ = os.Remove(store.partPath(id))
	_ = os.Remove(store.readyPath(id))
}

func (store *taskPayloadStore) reservedBytesLocked() int64 {
	var total int64
	for _, transfer := range store.transfers {
		total += transfer.TotalBytes
	}
	return total
}

func (store *taskPayloadStore) validTransferPath(id string) bool {
	return filepath.Base(id) == id && uuid.Validate(id) == nil
}

func (store *taskPayloadStore) validLoadedTransfer(metadataName string, transfer *taskPayloadTransfer) bool {
	if transfer == nil || !store.validTransferPath(transfer.TransferID) || metadataName != transfer.TransferID+".json" ||
		uuid.Validate(transfer.ProjectID) != nil || !validTaskPayloadTarget(transfer.TargetMethod) ||
		transfer.TotalBytes <= maximumRPCPayload || transfer.TotalBytes > maximumTaskDefinitionBytes ||
		!validSHA256Hex(transfer.SHA256) || transfer.AcceptedOffset < 0 || transfer.AcceptedOffset > transfer.TotalBytes ||
		transfer.ExpiresAt.IsZero() || !store.now().UTC().Before(transfer.ExpiresAt) {
		return false
	}
	path := store.partPath(transfer.TransferID)
	wantSize := transfer.AcceptedOffset
	if transfer.Committed {
		if transfer.AcceptedOffset != transfer.TotalBytes || strings.TrimSpace(transfer.IdempotencyKey) == "" {
			return false
		}
		path = store.readyPath(transfer.TransferID)
		wantSize = transfer.TotalBytes
	} else if transfer.IdempotencyKey != "" {
		return false
	}
	info, err := os.Lstat(path)
	if !transfer.Committed && errors.Is(err, os.ErrNotExist) && transfer.AcceptedOffset == transfer.TotalBytes {
		readyInfo, readyErr := os.Lstat(store.readyPath(transfer.TransferID))
		readyDigest, digestErr := sha256File(store.readyPath(transfer.TransferID))
		if readyErr == nil && readyInfo.Mode().IsRegular() && readyInfo.Size() == transfer.TotalBytes &&
			digestErr == nil && readyDigest == transfer.SHA256 &&
			os.Rename(store.readyPath(transfer.TransferID), path) == nil {
			info, err = os.Lstat(path)
		}
	}
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if !transfer.Committed && info.Size() > wantSize && info.Size() <= transfer.TotalBytes {
		if os.Truncate(path, wantSize) != nil {
			return false
		}
		info, err = os.Lstat(path)
	}
	if err != nil || info.Size() != wantSize {
		return false
	}
	if transfer.Committed {
		digest, err := sha256File(path)
		return err == nil && digest == transfer.SHA256
	}
	return true
}

func (store *taskPayloadStore) removeLoadedArtifacts(metadataName, transferID string) {
	_ = os.Remove(filepath.Join(store.root, metadataName))
	if store.validTransferPath(transferID) {
		_ = os.Remove(store.metaPath(transferID))
		_ = os.Remove(store.metaPath(transferID) + ".tmp")
		_ = os.Remove(store.partPath(transferID))
		_ = os.Remove(store.readyPath(transferID))
	}
}

func (store *taskPayloadStore) cleanupOrphanedLocked() {
	entries, err := os.ReadDir(store.root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		id := ""
		for _, suffix := range []string{".json.tmp", ".part", ".ready"} {
			if strings.HasSuffix(name, suffix) {
				id = strings.TrimSuffix(name, suffix)
				break
			}
		}
		if !store.validTransferPath(id) {
			continue
		}
		transfer := store.transfers[id]
		expected := ""
		if transfer != nil {
			if transfer.Committed {
				expected = filepath.Base(store.readyPath(id))
			} else {
				expected = filepath.Base(store.partPath(id))
			}
		}
		if name != expected {
			_ = os.Remove(filepath.Join(store.root, name))
		}
	}
}

func (store *taskPayloadStore) metaPath(id string) string {
	return filepath.Join(store.root, id+".json")
}
func (store *taskPayloadStore) partPath(id string) string {
	return filepath.Join(store.root, id+".part")
}
func (store *taskPayloadStore) readyPath(id string) string {
	return filepath.Join(store.root, id+".ready")
}

func validTaskPayloadTarget(method string) bool {
	return (strings.HasPrefix(method, "task.") || strings.HasPrefix(method, "workflow.")) &&
		!strings.HasPrefix(method, "task.payload.") && methodScope(method) != ""
}

func onlyTaskPayloadFields(input rpcInput, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		set[field] = struct{}{}
	}
	for field := range input {
		if _, ok := set[field]; !ok {
			return false
		}
	}
	return true
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func decodeTaskPayloadBase64(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, errRPCInvalid
}
