package turn

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

type canonicalRecoveryRaceAttempts struct {
	lifecycle.Attempts
	before func()
	calls  int
}

func (a *canonicalRecoveryRaceAttempts) Open(ctx context.Context, spec attempt.Spec) (attempt.Record, error) {
	a.calls++
	if a.calls == 1 {
		a.before()
	}
	return a.Attempts.Open(ctx, spec)
}

func TestCanonicalRecoveryAdmissionRefreshesAnEmptyRefusal(t *testing.T) {
	c, p, source, _ := recoveryCopyFixture(t, true)
	tracked, err := c.tasks.Create(task.Task{Channel: "console:canonical-race", Transport: "console", Member: "worker", ProjectID: p.ID, Workspace: p.Home.Path})
	if err != nil {
		t.Fatal(err)
	}
	req := Request{ConversationID: "console:canonical-race", MessageID: "canonical-race", SenderOpenID: "owner"}
	if _, err := c.tasks.BeginTurn(tracked.ID, "worker", "node", task.TurnInput{TurnID: req.MessageID, Address: channel.Address{Conversation: req.ConversationID, Channel: "console", Message: req.MessageID}}); err != nil {
		t.Fatal(err)
	}
	token, err := c.tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := c.executions.Begin(t.Context(), execution.Key{TaskID: tracked.ID, InstanceID: req.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Finish(nil)
	ctx := scope.Context()
	selected := agent.Agent{ID: "worker", Node: p.Home.Node, Harness: "mock"}
	spec, _, err := c.turnSpec(ctx, req, selected, tracked.ID, project.Binding{ProjectID: p.ID}, p.Canonical())
	if err != nil || spec.WorkspaceRecovery != nil {
		t.Fatalf("canonical input: %+v %v", spec, err)
	}
	spec.ID = "one-canonical-input"
	turn := &chatTurn{c: c, req: req, selected: selected, tracked: tracked.ID, binding: project.Binding{ProjectID: p.ID}, workspace: p.Canonical(), clock: newTurnClock(), spent: &turnSpend{resetIdle: func() {}}}
	raced := &canonicalRecoveryRaceAttempts{Attempts: c.attempts, before: func() {
		if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1); err != nil {
			t.Fatal(err)
		}
		if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), source.ID); err != nil {
			t.Fatal(err)
		}
	}}
	admission := recoveryAdmissionAttempts{waitingAttempts: waitingAttempts{Attempts: raced, passes: func(error) bool { return false }}, turn: turn}
	got, err := admission.Open(ctx, spec)
	if err != nil || got.ID != spec.ID || got.WorkspaceRecovery == nil || got.Workspace.Kind != project.KindWorktree || got.Workspace.Path == p.Home.Path || !turn.recoveryRefreshed || raced.calls != 2 {
		t.Fatalf("canonical refusal rejected an unadmitted input: %+v refreshed=%v opens=%d err=%v", got, turn.recoveryRefreshed, raced.calls, err)
	}
	if got.Execution == nil || *got.Execution != token || got.TaskID != tracked.ID || got.TurnID != req.MessageID {
		t.Fatal("refresh replaced the task or execution identity")
	}
	if err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error {
		return attempt.RecoveryHoldTx(tx, p.Home.Node, p.Home.Path)
	}); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("refresh removed the original hold: %v", err)
	}
}

func TestRecoveryReleaseClearsThePreviousDiagnostic(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	publishBoundRecoveryTurn(t, c, p, ws, "diagnostic-source", map[string]string{"accepted": "preserved\n"})
	episode := captureBoundCopy(t, c, source)
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		if err := c.attempts.RecordRecoveryWait(ctx, episode.ID, driver, errors.New("previous physical conflict")); err != nil {
			return err
		}
		if _, err := c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver); err != nil {
			return err
		}
		released, err := c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
		if err != nil || released.Error != "" {
			t.Fatalf("successful release retained a stale diagnostic: %+v %v", released, err)
		}
		return c.attempts.RecordRecoveryWait(ctx, episode.ID, driver, errors.New("new cleanup refusal"))
	}); err != nil {
		t.Fatal(err)
	}
	current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || current.Phase != "released" || current.Error != "new cleanup refusal" {
		t.Fatalf("cleanup could not record its own later diagnostic: %+v %v", current, err)
	}
}
