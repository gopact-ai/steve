package delegate

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type lostDelegateOpen struct {
	*fakeSessions
	attempts *attempt.Service
	tasks    *task.Store
	called   chan attempt.Record
}

func (s *lostDelegateOpen) OpenSession(ctx context.Context, _ harness.Placement, _, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	key, ok := execution.KeyOf(ctx)
	if !ok {
		return nil, errors.New("test lacks original execution scope")
	}
	r, err := s.attempts.Get(ctx, key.AttemptID)
	if err != nil {
		return nil, err
	}
	tracked, ok := s.tasks.Get(r.TaskID)
	if !ok || r.Execution == nil {
		return nil, errors.New("test lacks original task authorization")
	}
	s.called <- r
	return nil, &harness.NodeSessionOpenUncertain{Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: r.Execution.Epoch}, OpenCommandID: attempt.InputCommandID(r) + "/open", Cause: io.ErrUnexpectedEOF}
}

func TestDelegateLostNodeOpenRemainsPendingAcrossCoordinatorLifetime(t *testing.T) {
	w, _ := executionWorld(t)
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	registry := execution.New(lifetime, w.tasks)
	w.service.SetExecution(registry)
	w.service.SetGate(nil)
	parent := w.running(t, "codex")
	lost := &lostDelegateOpen{fakeSessions: w.sessions, attempts: w.attempts, tasks: w.tasks, called: make(chan attempt.Record, 2)}
	w.service.sessions = lost
	questions := make(chan RecoveryQuestion, 4)
	w.service.SetRecoveryQuestion(func(ctx context.Context, q RecoveryQuestion) (view.Answer, error) {
		questions <- q
		<-ctx.Done()
		return view.Answer{}, ctx.Err()
	})
	child, err := w.service.Start(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "original child task", Agent: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	var opened attempt.Record
	select {
	case opened = <-lost.called:
	case <-time.After(3 * time.Second):
		t.Fatal("test never attempted the native open")
	}
	if opened.SessionSettled == nil || *opened.SessionSettled {
		t.Fatal("node open was sent before its unknown-execution marker was durable")
	}
	select {
	case q := <-questions:
		if q.ParentTask != parent.ID || q.Task != child.TaskID || q.Attempt != opened.ID || q.Session != "" {
			t.Fatalf("pending open question changed original identity: %+v", q)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parent never learned native creation was pending")
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		w.service.mu.Lock()
		pending := w.service.pending[child.TaskID]
		w.service.mu.Unlock()
		if pending == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old preparation observer did not detach")
		}
		time.Sleep(time.Millisecond)
	}
	record, err := w.attempts.Get(t.Context(), opened.ID)
	tracked, _ := w.tasks.Get(child.TaskID)
	if err != nil || !record.Unsettled || record.Session != "" || record.State.Terminal() || tracked.Result != nil || tracked.State != task.StateRunning || !tracked.Attempts[0].Open() {
		t.Fatalf("unknown native open was falsely settled: record=%+v task=%+v err=%v", record, tracked, err)
	}
	if err := registry.Stop([]string{tracked.ID}, task.ErrExecutionStopped).Wait(t.Context()); !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("explicit Stop claimed native preparation had ended: %v", err)
	}
	cancel()
	cleanup, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	if err := registry.Shutdown(cleanup); err != nil {
		t.Fatalf("whole generation could not join preparation observer: %v", err)
	}
	restarted := recoveredDelegateService(t, w, lost)
	restarted.SetRecoveryQuestion(func(_ context.Context, q RecoveryQuestion) (view.Answer, error) {
		questions <- q
		return view.Answer{Value: "wait"}, nil
	})
	if err := restarted.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case q := <-questions:
		if q.Attempt != opened.ID || q.ParentTask != parent.ID || q.Session != "" {
			t.Fatalf("restart replaced original preparation: %+v", q)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart hid pending preparation from original parent")
	}
	select {
	case <-lost.called:
		t.Fatal("recovery reopened an uncertain native session")
	default:
	}
}
