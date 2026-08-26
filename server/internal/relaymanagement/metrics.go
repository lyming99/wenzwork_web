package relaymanagement

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OperationalMetrics returns bounded-cardinality management-plane metrics.
// It deliberately exposes aggregate states only, never installation, user,
// device, certificate, or operation identifiers.
func (store *Store) OperationalMetrics(ctx context.Context) (OperationalMetrics, error) {
	now := store.now().UTC()
	metrics := OperationalMetrics{
		Installations: make(map[string]int64), Instances: make(map[string]int64),
		AccessKeys: make(map[string]int64), EnrollmentTokens: make(map[string]int64), Certificates: make(map[string]int64),
		Operations: make(map[string]int64),
	}
	type stateCount struct {
		State string `gorm:"column:state"`
		Count int64  `gorm:"column:count"`
	}
	queries := []struct {
		name   string
		query  string
		args   []any
		target map[string]int64
	}{
		{name: "installations", query: "SELECT status AS state, count(*) AS count FROM relay_node_installations GROUP BY status", target: metrics.Installations},
		{name: "instances", query: `SELECT CASE WHEN status IN ('starting', 'ready', 'draining') AND lease_expires_at <= ? THEN 'lease_expired' ELSE status END AS state,
			count(*) AS count FROM relay_node_instances GROUP BY state`, args: []any{now}, target: metrics.Instances},
		{name: "access keys", query: "SELECT status AS state, count(*) AS count FROM relay_node_access_keys GROUP BY status", target: metrics.AccessKeys},
		{name: "enrollment tokens", query: "SELECT status AS state, count(*) AS count FROM relay_node_enrollment_tokens GROUP BY status", target: metrics.EnrollmentTokens},
		{name: "certificates", query: `SELECT CASE
			WHEN status = 'active' AND not_after <= ? THEN 'expired'
			WHEN status = 'active' AND not_after <= ? THEN 'expiring_24h'
			ELSE status END AS state, count(*) AS count
			FROM relay_node_certificates GROUP BY state`, args: []any{now, now.Add(24 * time.Hour)}, target: metrics.Certificates},
		{name: "operations", query: "SELECT status AS state, count(*) AS count FROM relay_operations GROUP BY status", target: metrics.Operations},
	}
	for _, query := range queries {
		var rows []stateCount
		if err := store.db.WithContext(ctx).Raw(query.query, query.args...).Scan(&rows).Error; err != nil {
			return OperationalMetrics{}, fmt.Errorf("read Relay %s metrics: %w", query.name, err)
		}
		for _, row := range rows {
			query.target[row.State] = row.Count
		}
	}
	if err := store.db.WithContext(ctx).Raw("SELECT COALESCE(sum(failed_attempts), 0) FROM relay_node_enrollment_tokens").Scan(&metrics.EnrollmentFailures).Error; err != nil {
		return OperationalMetrics{}, fmt.Errorf("read Relay enrollment failure metrics: %w", err)
	}
	var oldest sql.NullTime
	if err := store.db.WithContext(ctx).Raw("SELECT min(created_at) FROM relay_operations WHERE status IN ('pending', 'running')").Row().Scan(&oldest); err != nil {
		return OperationalMetrics{}, fmt.Errorf("read Relay operation age metrics: %w", err)
	}
	if oldest.Valid && now.After(oldest.Time) {
		metrics.OldestOperationAge = now.Sub(oldest.Time)
	}
	var oldestHeartbeat sql.NullTime
	if err := store.db.WithContext(ctx).Raw(`SELECT
		count(*) FILTER (WHERE instance.lease_expires_at <= ?) AS lease_expired,
		min(instance.last_heartbeat_at) AS oldest_heartbeat
		FROM relay_node_installations installation
		JOIN relay_node_instances instance ON instance.id = installation.current_instance_id
		WHERE installation.status IN ('enrolled', 'pending_activation', 'active', 'draining')`, now).
		Row().Scan(&metrics.CurrentLeaseExpired, &oldestHeartbeat); err != nil {
		return OperationalMetrics{}, fmt.Errorf("read Relay current heartbeat metrics: %w", err)
	}
	if oldestHeartbeat.Valid && now.After(oldestHeartbeat.Time) {
		metrics.OldestCurrentHeartbeatAge = now.Sub(oldestHeartbeat.Time)
	}
	if err := store.db.WithContext(ctx).Raw(`SELECT cell.code, cell.status, cell.connection_hard_limit,
		COALESCE(sum(instance.active_connections) FILTER (
			WHERE instance.status IN ('ready', 'draining') AND instance.lease_expires_at > ?), 0) AS active_connections,
		count(instance.id) FILTER (WHERE instance.status = 'ready' AND instance.lease_expires_at > ?) AS healthy_instances
		FROM relay_cells cell
		LEFT JOIN relay_node_instances instance ON instance.cell_id = cell.id
		GROUP BY cell.id ORDER BY cell.code`, now, now).Scan(&metrics.Cells).Error; err != nil {
		return OperationalMetrics{}, fmt.Errorf("read Relay Cell metrics: %w", err)
	}
	return metrics, nil
}
