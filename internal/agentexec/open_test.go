package agentexec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/roster"
)

type uncertainOpenSessions struct {
	*testSessions
	w     *testWorld
	opens int
}

func (s *uncertainOpenSessions) OpenSession(ctx context.Context, _ harness.Placement, _, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	s.opens++
	key, _ := execution.KeyOf(ctx)
	r, err := s.w.attempts.Get(ctx, key.AttemptID)
	if err != nil {
		return nil, err
	}
	if r.SessionSettled == nil || *r.SessionSettled {
		return nil, errors.New("node open was not durably armed")
	}
	return nil, &harness.NodeSessionOpenUncertain{Binding: nodewire.SessionBinding{TaskID: r.TaskID, AttemptID: r.ID, ProjectID: r.Project, NodeID: r.Node, TaskEpoch: r.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(r)}, OpenCommandID: attempt.InputCommandID(r) + "/open", Cause: errors.New("open reply lost")}
}

func TestUncertainAuxiliaryOpenDetachesObserverWithoutReplayingOrSettling(t *testing.T) {
	w := world(t, 0)
	cat, err := agent.NewCatalog(map[string]agent.Config{"agent": {Harness: "mock", Node: "worker", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	fleet := roster.New(cat)
	fleet.SetNodes(retainedAuxNodes{})
	w.runner.roster = fleet
	sessions := &uncertainOpenSessions{testSessions: &testSessions{}, w: w}
	w.runner.sessions = sessions
	lifetime, stop := context.WithCancel(t.Context())
	w.runner.executions = execution.New(lifetime, w.tasks)
	for range 2 {
		_, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindPlan), "original planning input", nil)
		var blocked *RecoveryBlocked
		if !errors.As(err, &blocked) {
			t.Fatalf("open uncertainty lost: %v", err)
		}
	}
	tracked, _ := w.tasks.Get(w.work.ID)
	if sessions.opens != 1 || tracked.Budget.Turns != 1 || len(tracked.Attempts) != 1 || !tracked.Attempts[0].Open() {
		t.Fatalf("uncertain open replayed or falsely settled: opens=%d task=%+v", sessions.opens, tracked)
	}
	stop()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := w.runner.executions.Shutdown(ctx); err != nil {
		t.Fatalf("node preparation blocked coordinator shutdown: %v", err)
	}
}

func TestKnownAuxiliarySessionWaitsForDurableIdentityBeforePrompt(t *testing.T) {
	w := world(t, 0)
	cat, err := agent.NewCatalog(map[string]agent.Config{"agent": {Harness: "mock", Node: "worker", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	fleet := roster.New(cat)
	fleet.SetNodes(retainedAuxNodes{})
	w.runner.roster = fleet
	sessions := &retainedAuxSessions{w: w, entered: make(chan struct{})}
	w.runner.sessions = sessions
	lifetime, stop := context.WithCancel(t.Context())
	defer stop()
	w.runner.executions = execution.New(lifetime, w.tasks)
	if err := w.book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_session BEFORE UPDATE OF state ON operations WHEN NEW.kind='attempt' AND NEW.state='running' BEGIN SELECT RAISE(FAIL,'session identity unavailable'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result, err := w.runner.Prompt(lifetime, w.spec(attempt.KindPlan), "original planning input", nil)
	var blocked *RecoveryBlocked
	if !errors.As(err, &blocked) || blocked.AttemptID == "" || result.Attempt.ID == "" || sessions.prompted != 0 {
		t.Fatalf("identity failure lost its execution or dispatched prompt: result=%+v err=%v prompts=%d", result, err, sessions.prompted)
	}
	record, err := w.attempts.Get(t.Context(), result.Attempt.ID)
	if err != nil || record.Session != "" || record.State != attempt.Prepared || !record.Unsettled {
		t.Fatalf("uncommitted session identity became a persisted fact: record=%+v err=%v", record, err)
	}
	tracked, _ := w.tasks.Get(w.work.ID)
	if !tracked.Attempts[0].Open() {
		t.Fatal("unknown native preparation was charged as settled")
	}
	stop()
	cleanup, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := w.runner.executions.Shutdown(cleanup); err != nil {
		t.Fatalf("known node preparation blocked coordinator shutdown: %v", err)
	}
}
