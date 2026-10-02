package turn

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func abandonInputFixture(t *testing.T, kind attempt.Kind, origin, anchor string, wrongTurn bool) (*Coordinator, attempt.Record) {
	t.Helper()
	c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
	tracked, err := tasks.Create(task.Task{Origin: origin, AnchorMessage: anchor, Transport: "console", Channel: "console:inputs", Member: "worker", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(tracked.ID)
	turnID := "web-original"
	if kind == attempt.KindPlan {
		turnID = fmt.Sprintf("plan/%s/r1/prompt/1", tracked.ID)
	}
	if wrongTurn {
		turnID = "plan/other/r1/prompt/1"
	}
	r, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "abandon-original", Kind: kind, TaskID: tracked.ID, TurnID: turnID, Execution: &token, Project: "p", Node: "node", Harness: "mock", Agent: "worker", Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "original", Project: "p", Node: "node", Path: t.TempDir(), Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = c.attempts.Advance(t.Context(), r.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_original" })
		if err != nil {
			t.Fatal(err)
		}
	}
	return c, r
}
func finishAndReanchor(t *testing.T, c *Coordinator, r attempt.Record) {
	t.Helper()
	if _, err := c.attempts.Finish(t.Context(), r.ID, "fixture", attempt.Result{}); err != nil {
		t.Fatal(err)
	}
	if err := c.tasks.SettleAttempt(t.Context(), r.TaskID, r.ID, r.TurnID, time.Now(), task.OutcomeOK, task.RecoveryUsage{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.BeginTurn(r.TaskID, r.Agent, r.Node, task.TurnInput{Address: channel.Address{Channel: "console", Conversation: "console:inputs", Message: "web-newer"}, TurnID: "web-newer"}); err != nil {
		t.Fatal(err)
	}
}
func exhaustAbandonInput(t *testing.T, c *Coordinator, r attempt.Record) {
	t.Helper()
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "fixture", attempt.ErrStopConfirmationRequired, nil); err != nil {
		t.Fatal(err)
	}
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
}
func TestAbandonmentKeepsTheOriginalInputAfterTheTaskReanchors(t *testing.T) {
	for _, kind := range []attempt.Kind{attempt.KindChat, attempt.KindPlan} {
		t.Run(string(kind), func(t *testing.T) {
			origin := "chat"
			if kind == attempt.KindPlan {
				origin = "plan"
			}
			c, r := abandonInputFixture(t, kind, origin, "web-original", false)
			finishAndReanchor(t, c, r)
			exhaustAbandonInput(t, c, r)
			got, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 1)
			if err != nil {
				t.Fatal(err)
			}
			if got.Abandoned.MessageID != "web-original" {
				t.Fatalf("original turn=%s abandonment input=%s", r.TurnID, got.Abandoned.MessageID)
			}
		})
	}
}
func TestAbandonPlanWithNoExactOriginalInputIsRefusedWithoutSideEffects(t *testing.T) {
	for _, which := range []string{"missing input", "wrong kind of task", "wrong plan turn", "old record"} {
		t.Run(which, func(t *testing.T) {
			origin, anchor := "plan", "web-original"
			if which == "missing input" {
				anchor = ""
			}
			if which == "wrong kind of task" {
				origin = "chat"
			}
			c, r := abandonInputFixture(t, attempt.KindPlan, origin, anchor, which == "wrong plan turn")
			exhaustAbandonInput(t, c, r)
			if which == "old record" {
				if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
					_, err := tx.Exec(`UPDATE bindings SET data=json_remove(data,'$.plan_message_id') WHERE kind='task' AND id=?`, r.TaskID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := c.tasks.Get(r.TaskID)
			original, _ := c.attempts.Get(t.Context(), r.ID)
			_, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 1)
			var user UserError
			if !errors.Is(err, attempt.ErrAbandonInput) || !errors.As(err, &user) {
				t.Fatalf("unproven plan input accepted or unexplained: %v", err)
			}
			after, _ := c.tasks.Get(r.TaskID)
			current, _ := c.attempts.Get(t.Context(), r.ID)
			if !sameAbandonJSON(before, after) || !sameAbandonJSON(original, current) {
				t.Fatal("refused original-input proof changed task or execution")
			}
		})
	}
}
