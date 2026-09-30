package app

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

type forceSessions struct {
	mu              sync.Mutex
	states          map[string]nodewire.SessionState
	killed, aborted []string
	failure         error
}

func (s *forceSessions) AttachRetainedSession(_ context.Context, _ harness.Placement, id, _ string) (harness.ResumableRunner, error) {
	return forceRunner{s: s, id: id}, nil
}

type forceRunner struct {
	harness.ResumableRunner
	s  *forceSessions
	id string
}

func (r forceRunner) InspectRetained(context.Context) (nodewire.SessionState, error) {
	return r.s.states[r.id], nil
}
func (r forceRunner) StopRetained(context.Context) (nodewire.SessionState, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.aborted = append(r.s.aborted, r.id)
	st := r.s.states[r.id]
	st.ProcessStopped = true
	return st, nil
}
func (r forceRunner) KillRetained(context.Context) (nodewire.SessionState, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.killed = append(r.s.killed, r.id)
	st := r.s.states[r.id]
	st.ProcessStopped = r.s.failure == nil
	return st, r.s.failure
}
func forceFixture(t *testing.T, n int) (*applicationStops, []attempt.Record, *forceSessions) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	as := attempt.New(book)
	sessions := &forceSessions{states: map[string]nodewire.SessionState{}}
	var records []attempt.Record
	for i := range n {
		tracked, err := tasks.Create(task.Task{Channel: "console:force", Member: "worker", ProjectID: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
			t.Fatal(err)
		}
		token, _ := tasks.ExecutionToken(tracked.ID)
		r, err := as.Open(t.Context(), attempt.Spec{ID: fmt.Sprintf("attempt-%02d", i), TaskID: tracked.ID, TurnID: fmt.Sprintf("turn-%d", i), Kind: attempt.KindChat, Project: "p", Node: "node", Harness: "mock", Agent: "worker", Execution: &token, Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: fmt.Sprint(i), Project: "p", Node: "node", Path: t.TempDir(), Kind: project.KindWorktree}})
		if err != nil {
			t.Fatal(err)
		}
		if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
			t.Fatal(err)
		}
		for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
			r, err = as.Advance(t.Context(), r.ID, phase, "test", func(r *attempt.Record) { r.Session = "ns_" + r.ID })
			if err != nil {
				t.Fatal(err)
			}
		}
		sessions.states[r.Session] = nodewire.SessionState{ID: r.Session, Harness: r.Harness, Binding: sessionBinding(r, tracked), State: nodewire.SessionClosed}
		_, _ = tasks.SetAside(tracked.ID, task.StateCancelled)
		records = append(records, r)
	}
	return newApplicationStops(as, tasks, sessions, i18n.New(i18n.LocaleEN)), records, sessions
}
func TestForceStopConfirmsAndProjectsAccounting(t *testing.T) {
	s, records, sessions := forceFixture(t, 1)
	r := records[0]
	if _, err := s.attempts.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.attempts.Get(t.Context(), r.ID)
	if len(sessions.killed) != 1 || len(sessions.aborted) != 0 || got.ForceStop.Level != "confirmed" || !got.StopProjected || got.Unsettled {
		t.Fatalf("not a confirmed kill: kill=%v abort=%v record=%+v", sessions.killed, sessions.aborted, got)
	}
}
func TestForceStopPrioritizedOverOrdinaryRotation(t *testing.T) {
	s, records, sessions := forceFixture(t, 6)
	_, err := s.attempts.RequestForceStop(t.Context(), records[5].ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sessions.killed) != 1 || len(sessions.aborted) != 3 {
		t.Fatalf("force priority/budget: kill=%v abort=%v", sessions.killed, sessions.aborted)
	}
}
func TestForceStopClassifiedFailureDoesNotFallBackToAbort(t *testing.T) {
	s, records, sessions := forceFixture(t, 1)
	sessions.failure = &node.SessionError{Code: "stop_unproven", Message: "identity unknown"}
	_, err := s.attempts.RequestForceStop(t.Context(), records[0].ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.attempts.Get(t.Context(), records[0].ID)
	if len(sessions.killed) != 1 || len(sessions.aborted) != 0 || got.ForceStop.Level != "exhausted" || !got.Unsettled {
		t.Fatalf("unproven stop: %+v kill=%v abort=%v", got.ForceStop, sessions.killed, sessions.aborted)
	}
}

func TestForceStopReachesOriginalNodeAndLostOpen(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			f := newStopRegistryFixture(t, bin, pending)
			f.stops.sessions = f.sessions.manager
			f.owner.Finish(harness.ErrStopUnconfirmed)
			if _, err := f.attempts.RequestForceStop(f.ctx, f.record.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			if err := f.stops.Reconcile(f.ctx); err != nil {
				t.Fatal(err)
			}
			got, err := f.attempts.Get(f.ctx, f.record.ID)
			if err != nil || got.ForceStop.Level != "confirmed" || !got.StopProjected || got.Unsettled {
				t.Fatalf("force stop not confirmed: %+v %v", got, err)
			}
			f.requireResolved(t)
		})
	}
}
