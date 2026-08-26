package privacyguard

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestAuditAllowlistRejectsPeerAndFileContent(t *testing.T) {
	for _, key := range []string{
		"prompt", "response", "reply", "system_prompt", "tool_result", "command", "environment",
		"project_name", "project_path", "file_name", "relative_path", "attachment", "manifest",
		"manifest_hash", "content_hash", "digest", "payload", "ciphertext",
	} {
		_, err := FilterAuditAttributes(map[string]any{"transfer_id": "transfer-1", key: "private-marker"})
		if !errors.Is(err, ErrSensitiveAttribute) {
			t.Fatalf("key %q error = %v", key, err)
		}
	}
	filtered, err := FilterAuditAttributes(map[string]any{
		"transfer_id":      "transfer-1",
		"project_id":       "11111111-1111-4111-8111-111111111111",
		"ciphertext_bytes": 42,
		"result_code":      "completed",
	})
	if err != nil || len(filtered) != 4 {
		t.Fatalf("FilterAuditAttributes() = %v, %v", filtered, err)
	}
}

func TestAuditAllowlistRejectsSensitiveValuesHiddenUnderSafeKeys(t *testing.T) {
	for name, value := range map[string]any{
		"free text result":  "completed: C:/private/customer.txt",
		"newline injection": "completed\nprompt=private",
		"nested payload":    map[string]any{"prompt": "private"},
		"byte payload":      []byte("private"),
	} {
		_, err := FilterAuditAttributes(map[string]any{"result_code": value})
		if !errors.Is(err, ErrSensitiveAttribute) {
			t.Errorf("%s error = %v", name, err)
		}
	}
	for name, value := range map[string]any{
		"string byte count": "42",
		"floating duration": 1.5,
		"invalid timestamp": "yesterday",
	} {
		key := "ciphertext_bytes"
		if name == "floating duration" {
			key = "duration_ms"
		} else if name == "invalid timestamp" {
			key = "started_at"
		}
		_, err := FilterAuditAttributes(map[string]any{key: value})
		if !errors.Is(err, ErrSensitiveAttribute) {
			t.Errorf("%s error = %v", name, err)
		}
	}
}

func TestCloudContractsContainNoPeerOrFileBodyPersistence(t *testing.T) {
	root := repositoryRoot(t)
	migrationFiles, err := filepath.Glob(filepath.Join(root, "server", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	forbiddenTables := []string{"conversation_messages", "ai_runs", "peer_messages", "peer_file_manifests", "peer_file_chunks"}
	for _, path := range migrationFiles {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(contents))
		for _, table := range forbiddenTables {
			if strings.Contains(lower, "create table "+table) {
				t.Fatalf("%s creates forbidden cloud content table %s", path, table)
			}
		}
	}

	fileProto, err := os.ReadFile(filepath.Join(root, "api", "remote", "v1", "file.proto"))
	if err != nil {
		t.Fatal(err)
	}
	cleartextField := regexp.MustCompile(`(?m)^\s*(?:string|bytes)\s+(?:file_name|relative_path|absolute_path|manifest_hash|content_hash|plaintext|payload)\s*=`)
	if match := cleartextField.Find(fileProto); match != nil {
		t.Fatalf("file.proto exposes sensitive field %q", match)
	}
}

func TestCloudTaskContractIsProjectionOnlyAndRetainsMetadataCancellation(t *testing.T) {
	root := repositoryRoot(t)
	privacyMigration, err := os.ReadFile(filepath.Join(root, "server", "migrations", "00039_remote_task_privacy_boundary.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up := strings.ToLower(strings.SplitN(string(privacyMigration), "-- +goose Down", 2)[0])
	for _, required := range []string{
		"drop table if exists remote_task_logs",
		"alter table remote_tasks drop column input_metadata",
		"delete from remote_commands where kind = 'task.create'",
		"check (kind in ('project.sync', 'task.cancel'))",
		"remote_commands_projection_body_check",
		"body = jsonb_build_object('taskid', task_id)",
		"remote_tasks_projection_type_check",
		"remote_tasks_projection_title_check",
	} {
		if !strings.Contains(up, required) {
			t.Errorf("privacy migration is missing %q", required)
		}
	}

	openAPI, err := os.ReadFile(filepath.Join(root, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	contract := strings.ToLower(string(openAPI))
	for _, forbidden := range []string{
		"deviceremotetasklog", "remotetasklogentry", "remotetasklogpage", "createremotetaskrequest", "task.log",
		"/remote/tasks/{taskid}/logs", "/remote/tasks/{taskid}/retries",
		"kind=task.create", "project.sync, task.create, task.cancel",
	} {
		if strings.Contains(contract, forbidden) {
			t.Errorf("OpenAPI still exposes cloud task body contract %q", forbidden)
		}
	}

	remoteTasks, err := os.ReadFile(filepath.Join(root, "server", "internal", "remotecontrol", "tasks.go"))
	if err != nil {
		t.Fatal(err)
	}
	deviceEvents, err := os.ReadFile(filepath.Join(root, "server", "internal", "remotecontrol", "device.go"))
	if err != nil {
		t.Fatal(err)
	}
	cloudImplementation := strings.ToLower(string(remoteTasks) + "\n" + string(deviceEvents))
	for _, forbidden := range []string{
		"insert into remote_task_logs", `"kind": "task.create"`, `kind: "task.create"`,
	} {
		if strings.Contains(cloudImplementation, forbidden) {
			t.Errorf("cloud implementation still writes %q", forbidden)
		}
	}

	webAPI, err := os.ReadFile(filepath.Join(root, "web", "src", "api", "remote.ts"))
	if err != nil {
		t.Fatal(err)
	}
	webPanel, err := os.ReadFile(filepath.Join(root, "web", "src", "components", "remote", "RemoteTasksPanel.vue"))
	if err != nil {
		t.Fatal(err)
	}
	web := string(webAPI) + "\n" + string(webPanel)
	for _, forbidden := range []string{"createRemoteTask", "retryRemoteTask", "listRemoteTaskLogs", "/logs", "/retries"} {
		if strings.Contains(web, forbidden) {
			t.Errorf("Web still uses deprecated cloud task body API %q", forbidden)
		}
	}
	if !strings.Contains(web, "cancelRemoteTask") {
		t.Error("Web lost the metadata-only reliable cancellation path")
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve current test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
}
