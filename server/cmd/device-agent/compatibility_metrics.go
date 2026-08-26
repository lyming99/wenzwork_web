package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

const maximumCompatibilityMetricDuration = 15 * time.Minute
const compatibilityMetricWriteBudget = 250 * time.Millisecond
const maximumCompatibilityMetricValue = uint64(1<<63 - 1)

type compatibilityMetricBucket struct {
	CapabilityVersion         string `json:"capabilityVersion"`
	ErrorCode                 string `json:"errorCode"`
	CallCount                 uint64 `json:"callCount"`
	TotalDurationMilliseconds uint64 `json:"totalDurationMilliseconds"`
	firstObservedAt           time.Time
	lastObservedAt            time.Time
}

type compatibilityMetricKey struct {
	CapabilityVersion string
	ErrorCode         string
}

type compatibilityMetricWrite struct {
	compatibilityMetricKey
	CallCount                 uint64
	TotalDurationMilliseconds uint64
	FirstObservedAt           time.Time
	LastObservedAt            time.Time
}

func compatibilityCapabilityVersion(d dispatcher, method, projectID string) string {
	if !d.enforceProjectBinding {
		return ""
	}
	projectID = strings.TrimSpace(projectID)
	switch {
	case strings.HasPrefix(method, "file."):
		if projectID == "" {
			return "files.v1"
		}
		return "files.v2"
	case method == "terminal.execute":
		return "terminal.v1"
	case strings.HasPrefix(method, "terminal."):
		return "terminal.v2"
	case strings.HasPrefix(method, "task.") || strings.HasPrefix(method, "workflow."):
		if d.scope == "remote.peer.task.control" && projectID != "" {
			return "tasks.v2"
		}
		return "tasks.v1"
	case strings.HasPrefix(method, "ai.config."):
		return "ai.v2"
	case strings.HasPrefix(method, "conversation.") || strings.HasPrefix(method, "chat."):
		if projectID == "" || method == "conversation.chat.send" || method == "chat.send" {
			return "ai.v1"
		}
		return "ai.v2"
	default:
		return ""
	}
}

func compatibilityMetricErrorCode(response *remotev1.RpcResponse) string {
	if response == nil || response.GetError() == nil {
		return "ok"
	}
	return response.GetError().GetCode().String()
}

func (d dispatcher) recordCompatibilityMetricBestEffort(
	capabilityVersion string,
	errorCode string,
	duration time.Duration,
) {
	if capabilityVersion == "" || d.state == nil || d.state.business == nil {
		return
	}
	d.state.business.enqueueCompatibilityMetric(
		capabilityVersion,
		errorCode,
		duration,
		time.Now().UTC(),
	)
}

// enqueueCompatibilityMetric intentionally returns before SQLite work starts.
// Compatibility aggregates are operational telemetry, not a dependency of an
// encrypted RPC response. A single worker coalesces bounded dimensions, so a
// busy local writer cannot create an unbounded goroutine or make list RPCs
// wait behind audit I/O.
func (store *businessStore) enqueueCompatibilityMetric(capabilityVersion, errorCode string, duration time.Duration, observedAt time.Time) {
	write, ok := newCompatibilityMetricWrite(capabilityVersion, errorCode, duration, observedAt)
	if !ok || store == nil {
		return
	}
	store.compatibilityMetricsMu.Lock()
	if store.pendingCompatibilityMetrics == nil {
		store.pendingCompatibilityMetrics = make(map[compatibilityMetricKey]compatibilityMetricWrite)
	}
	key := write.compatibilityMetricKey
	if current, found := store.pendingCompatibilityMetrics[key]; found {
		write = mergeCompatibilityMetricWrites(current, write)
	}
	store.pendingCompatibilityMetrics[key] = write
	if store.compatibilityMetricsFlushing {
		store.compatibilityMetricsMu.Unlock()
		return
	}
	store.compatibilityMetricsFlushing = true
	done := make(chan struct{})
	store.compatibilityMetricsDone = done
	store.compatibilityMetricsMu.Unlock()
	go store.flushCompatibilityMetrics(done)
}

func newCompatibilityMetricWrite(capabilityVersion, errorCode string, duration time.Duration, observedAt time.Time) (compatibilityMetricWrite, bool) {
	if !validCompatibilityCapabilityVersion(capabilityVersion) || !validCompatibilityMetricErrorCode(errorCode) || observedAt.IsZero() {
		return compatibilityMetricWrite{}, false
	}
	if duration < 0 {
		duration = 0
	}
	if duration > maximumCompatibilityMetricDuration {
		duration = maximumCompatibilityMetricDuration
	}
	return compatibilityMetricWrite{
		compatibilityMetricKey: compatibilityMetricKey{CapabilityVersion: capabilityVersion, ErrorCode: errorCode},
		CallCount:              1, TotalDurationMilliseconds: uint64(duration.Milliseconds()),
		FirstObservedAt: observedAt.UTC(), LastObservedAt: observedAt.UTC(),
	}, true
}

func mergeCompatibilityMetricWrites(current, next compatibilityMetricWrite) compatibilityMetricWrite {
	if current.CallCount > maximumCompatibilityMetricValue-next.CallCount {
		current.CallCount = maximumCompatibilityMetricValue
	} else {
		current.CallCount += next.CallCount
	}
	if current.TotalDurationMilliseconds > maximumCompatibilityMetricValue-next.TotalDurationMilliseconds {
		current.TotalDurationMilliseconds = maximumCompatibilityMetricValue
	} else {
		current.TotalDurationMilliseconds += next.TotalDurationMilliseconds
	}
	if next.FirstObservedAt.Before(current.FirstObservedAt) {
		current.FirstObservedAt = next.FirstObservedAt
	}
	if next.LastObservedAt.After(current.LastObservedAt) {
		current.LastObservedAt = next.LastObservedAt
	}
	return current
}

func (store *businessStore) flushCompatibilityMetrics(done chan struct{}) {
	for {
		store.compatibilityMetricsMu.Lock()
		writes := store.pendingCompatibilityMetrics
		store.pendingCompatibilityMetrics = make(map[compatibilityMetricKey]compatibilityMetricWrite)
		store.compatibilityMetricsMu.Unlock()
		for _, write := range writes {
			ctx, cancel := context.WithTimeout(context.Background(), compatibilityMetricWriteBudget)
			_ = store.recordCompatibilityMetricWrite(ctx, write)
			cancel()
		}
		store.compatibilityMetricsMu.Lock()
		empty := len(store.pendingCompatibilityMetrics) == 0
		if empty {
			// Transition to idle while holding the same mutex used by enqueue.
			// That prevents a late enqueue from being stranded after it observed
			// a still-running worker.
			store.compatibilityMetricsFlushing = false
			if store.compatibilityMetricsDone == done {
				store.compatibilityMetricsDone = nil
			}
			close(done)
			store.compatibilityMetricsMu.Unlock()
			return
		}
		store.compatibilityMetricsMu.Unlock()
	}
}

// waitForCompatibilityMetrics is used only by controlled shutdown/tests. It
// never belongs on the encrypted RPC response path.
func (store *businessStore) waitForCompatibilityMetrics(ctx context.Context) error {
	if store == nil {
		return errors.New("business store is unavailable")
	}
	store.compatibilityMetricsMu.Lock()
	done := store.compatibilityMetricsDone
	store.compatibilityMetricsMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validCompatibilityCapabilityVersion(value string) bool {
	switch value {
	case "files.v1", "files.v2", "terminal.v1", "terminal.v2", "tasks.v1", "tasks.v2", "ai.v1", "ai.v2":
		return true
	default:
		return false
	}
}

func validCompatibilityMetricErrorCode(value string) bool {
	if value == "ok" {
		return true
	}
	code, found := remotev1.RpcErrorCode_value[value]
	return found && remotev1.RpcErrorCode(code) != remotev1.RpcErrorCode_RPC_ERROR_CODE_UNSPECIFIED
}

func (store *businessStore) recordCompatibilityMetric(
	ctx context.Context,
	capabilityVersion string,
	errorCode string,
	duration time.Duration,
	observedAt time.Time,
) error {
	write, ok := newCompatibilityMetricWrite(capabilityVersion, errorCode, duration, observedAt)
	if !ok {
		return errors.New("RPC compatibility metric is invalid")
	}
	return store.recordCompatibilityMetricWrite(ctx, write)
}

func (store *businessStore) recordCompatibilityMetricWrite(ctx context.Context, write compatibilityMetricWrite) error {
	if store == nil || !validCompatibilityCapabilityVersion(write.CapabilityVersion) || !validCompatibilityMetricErrorCode(write.ErrorCode) ||
		write.CallCount == 0 || write.CallCount > maximumCompatibilityMetricValue || write.TotalDurationMilliseconds > maximumCompatibilityMetricValue ||
		write.FirstObservedAt.IsZero() || write.LastObservedAt.IsZero() || write.LastObservedAt.Before(write.FirstObservedAt) {
		return errors.New("RPC compatibility metric is invalid")
	}
	firstObservedAtMilliseconds := write.FirstObservedAt.UTC().UnixMilli()
	lastObservedAtMilliseconds := write.LastObservedAt.UTC().UnixMilli()

	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `INSERT INTO rpc_compatibility_metrics(
            capability_version, error_code, call_count, total_duration_ms, first_observed_at_ms, last_observed_at_ms
        ) VALUES(?, ?, ?, ?, ?, ?)
        ON CONFLICT(capability_version, error_code) DO UPDATE SET
            call_count = call_count + excluded.call_count,
            total_duration_ms = total_duration_ms + excluded.total_duration_ms,
            first_observed_at_ms = MIN(first_observed_at_ms, excluded.first_observed_at_ms),
            last_observed_at_ms = MAX(last_observed_at_ms, excluded.last_observed_at_ms)`,
		write.CapabilityVersion, write.ErrorCode, write.CallCount, write.TotalDurationMilliseconds, firstObservedAtMilliseconds, lastObservedAtMilliseconds)
	if err != nil {
		return fmt.Errorf("record RPC compatibility metric: %w", err)
	}
	return nil
}

func (store *businessStore) listCompatibilityMetrics(ctx context.Context) ([]compatibilityMetricBucket, error) {
	if store == nil {
		return nil, errors.New("business store is unavailable")
	}
	db, err := store.openReadDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT capability_version, error_code, call_count, total_duration_ms,
            first_observed_at_ms, last_observed_at_ms
        FROM rpc_compatibility_metrics
        ORDER BY capability_version, error_code`)
	if err != nil {
		return nil, fmt.Errorf("list RPC compatibility metrics: %w", err)
	}
	defer rows.Close()
	metrics := make([]compatibilityMetricBucket, 0)
	for rows.Next() {
		var metric compatibilityMetricBucket
		var callCount, totalDurationMilliseconds, firstObservedAt, lastObservedAt int64
		if err := rows.Scan(
			&metric.CapabilityVersion,
			&metric.ErrorCode,
			&callCount,
			&totalDurationMilliseconds,
			&firstObservedAt,
			&lastObservedAt,
		); err != nil {
			return nil, fmt.Errorf("scan RPC compatibility metric: %w", err)
		}
		if !validCompatibilityCapabilityVersion(metric.CapabilityVersion) ||
			!validCompatibilityMetricErrorCode(metric.ErrorCode) || callCount <= 0 ||
			totalDurationMilliseconds < 0 || lastObservedAt < firstObservedAt {
			return nil, errors.New("stored RPC compatibility metric is invalid")
		}
		metric.CallCount = uint64(callCount)
		metric.TotalDurationMilliseconds = uint64(totalDurationMilliseconds)
		metric.firstObservedAt = time.UnixMilli(firstObservedAt).UTC()
		metric.lastObservedAt = time.UnixMilli(lastObservedAt).UTC()
		metrics = append(metrics, metric)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate RPC compatibility metrics: %w", err)
	}
	return metrics, nil
}
