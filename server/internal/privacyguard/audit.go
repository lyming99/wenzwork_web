package privacyguard

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"strings"
	"time"
)

var ErrSensitiveAttribute = errors.New("sensitive remote content attribute is forbidden")

var allowedAuditAttributes = map[string]struct{}{
	"session_id": {}, "transfer_id": {}, "project_id": {}, "source_device_id": {}, "target_device_id": {},
	"started_at": {}, "ended_at": {}, "ciphertext_bytes": {}, "bytes_in": {}, "bytes_out": {},
	"duration_ms": {}, "result_code": {}, "error_code": {}, "scope": {}, "cell_id": {}, "node_id": {},
	"connection_epoch": {}, "assignment_version": {}, "grant_version": {},
}

var auditMachineValue = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// FilterAuditAttributes enforces an allowlist. Callers cannot accidentally add
// a prompt, response, file path, manifest, digest or payload to Relay audit
// records merely by choosing a new map key.
func FilterAuditAttributes(attributes map[string]any) (map[string]any, error) {
	filtered := make(map[string]any, len(attributes))
	for key, value := range attributes {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if _, ok := allowedAuditAttributes[normalized]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrSensitiveAttribute, key)
		}
		if !validAuditAttributeValue(normalized, value) {
			return nil, fmt.Errorf("%w: %s has an unsafe value", ErrSensitiveAttribute, key)
		}
		filtered[normalized] = value
	}
	return maps.Clone(filtered), nil
}

func validAuditAttributeValue(key string, value any) bool {
	switch key {
	case "started_at", "ended_at":
		switch typed := value.(type) {
		case time.Time:
			return !typed.IsZero()
		case string:
			_, err := time.Parse(time.RFC3339Nano, typed)
			return err == nil
		default:
			return false
		}
	case "ciphertext_bytes", "bytes_in", "bytes_out", "duration_ms", "connection_epoch", "assignment_version", "grant_version":
		if value == nil {
			return false
		}
		reflected := reflect.ValueOf(value)
		kind := reflected.Kind()
		if kind >= reflect.Int && kind <= reflect.Int64 {
			return reflected.Int() >= 0
		}
		return kind >= reflect.Uint && kind <= reflect.Uint64
	default:
		text, ok := value.(string)
		return ok && auditMachineValue.MatchString(text)
	}
}
