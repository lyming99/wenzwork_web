package remotecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCloudTaskBodyOperationsRequireE2EEPeer(t *testing.T) {
	service := &Service{}
	marker := json.RawMessage(`{"prompt":"must-not-enter-control-plane"}`)
	if _, _, err := service.CreateTask(context.Background(), CreateTaskInput{Input: marker}); !errors.Is(err, ErrPeerRequired) {
		t.Fatalf("CreateTask error = %v", err)
	}
	if _, _, err := service.RetryTask(context.Background(), RetryTaskInput{TaskID: uuid.New()}); !errors.Is(err, ErrPeerRequired) {
		t.Fatalf("RetryTask error = %v", err)
	}
	if _, err := service.ListTaskLogs(context.Background(), uuid.New(), uuid.New(), "stdout", 0, 1024); !errors.Is(err, ErrPeerRequired) {
		t.Fatalf("ListTaskLogs error = %v", err)
	}
	if _, err := service.enqueueCommand(context.Background(), uuid.New(), uuid.New(), nil, "task.create", "remote.task.write",
		map[string]any{"input": marker}, nil, "privacy-test-key"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("task.create enqueue error = %v", err)
	}
}
