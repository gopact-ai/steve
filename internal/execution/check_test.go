package execution

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestCheckExecutionUsesOriginalScopeEpochWithoutReadmittingStoppedWork(t *testing.T) {
	_, scope, tasks := stopTestScope(t, t.Context())
	defer scope.Finish(nil)
	if err := CheckExecution(scope.Context()); err != nil {
		t.Fatal(err)
	}
	detached, cancel := context.WithCancel(scope.Context())
	cancel()
	if err := CheckExecution(detached); err != nil {
		t.Fatalf("observer cancellation was mistaken for task revocation: %v", err)
	}
	if _, err := tasks.SetAside(scope.key.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if err := CheckExecution(scope.Context()); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("original scope did not observe revoked token: %v", err)
	}
	if _, err := tasks.Advance(scope.key.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := CheckExecution(scope.Context()); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("resumed task silently upgraded original scope epoch: %v", err)
	}
	if err := CheckExecution(WithProbeKey(scope.Context(), scope.key)); err == nil || errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("probe fabricated explicit stop authority: %v", err)
	}
	if err := CheckExecution(t.Context()); err == nil || errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("missing scope fabricated explicit stop authority: %v", err)
	}
}
