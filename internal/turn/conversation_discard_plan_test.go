package turn

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func discardPlanRun(t *testing.T, c *Coordinator, taskID, phase string) exec.RunRecord {
	t.Helper()
	tracked, found := c.tasks.Get(taskID)
	if !found {
		t.Fatal("missing fixture task")
	}
	token := task.ExecutionToken{TaskID: taskID, Epoch: tracked.ExecutionEpoch}
	p, err := c.plans.Create(plan.Plan{TaskID: taskID, ProjectID: "p", Goal: "finish work", By: "rule", Execution: &token, Steps: []plan.Step{{ID: "work", Goal: "work", Agent: "worker", Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "fixture"}}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err = c.plans.Revise(p.ID, p.Steps, "rule", "revised goal")
	if err != nil {
		t.Fatal(err)
	}
	rec := exec.RunRecord{ID: "plan-run/" + p.ID, PlanID: p.ID, Rev: p.Rev, TaskID: taskID, ProjectID: "p", RunID: "plan-" + p.ID + "-r2", Phase: phase, Execution: &token, Error: "task budget exhausted: turns", OpenedAt: time.Now().UTC(), Owner: "fixture"}
	if phase == exec.RunLanding {
		rec.Sinks = []exec.Sink{{StepID: "work", Artifact: "kept-artifact", LandingID: "kept-sink"}}
	}
	if phase == exec.RunCompleted {
		rec.Outcome, rec.Error = "success", ""
	}
	if _, err := ledgerOf(t, c).Begin(t.Context(), rec.ID, "plan-run", phase, "fixture", rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func discardPlanCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c := buildCoordinator(t)
	if err := c.projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	return c
}

func discardPlanTask(t *testing.T, c *Coordinator, channel, parent string) task.Task {
	t.Helper()
	tracked, err := c.tasks.Create(task.Task{Channel: channel, Parent: parent, Origin: "plan", ProjectID: "p", AnchorMessage: "original", Goal: "work"})
	if err != nil {
		t.Fatal(err)
	}
	return tracked
}

func readDiscardRun(t *testing.T, book *ledger.Ledger, id string) (ledger.Operation, exec.RunRecord) {
	t.Helper()
	op, found, err := book.Operation(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("run %s: found=%t err=%v", id, found, err)
	}
	var rec exec.RunRecord
	if err := json.Unmarshal(op.Data, &rec); err != nil {
		t.Fatal(err)
	}
	return op, rec
}

func TestDiscardConversationRetiresOnlyItsPlanRunsAndBinding(t *testing.T) {
	c := discardPlanCoordinator(t)
	root := discardPlanTask(t, c, "console:discard", "")
	child := discardPlanTask(t, c, "delegate:discard", root.ID)
	done := discardPlanTask(t, c, root.Channel, "")
	other := discardPlanTask(t, c, "console:keep", "")
	runs := []exec.RunRecord{discardPlanRun(t, c, root.ID, exec.RunExecuting), discardPlanRun(t, c, child.ID, exec.RunLanding)}
	completed := discardPlanRun(t, c, done.ID, exec.RunCompleted)
	unrelated := discardPlanRun(t, c, other.ID, exec.RunExecuting)
	book := ledgerOf(t, c)
	orphan := exec.RunRecord{ID: "plan-run/unknown-history", PlanID: "unknown-history", TaskID: "missing-task", Phase: exec.RunExecuting}
	if _, err := book.Begin(t.Context(), orphan.ID, "plan-run", orphan.Phase, "fixture", orphan); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{root.Channel, other.Channel} {
		if _, err := c.projects.Bind(t.Context(), channel, "p", "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	history := c.plans.Revisions(runs[0].PlanID)
	completedBefore, _ := readDiscardRun(t, book, completed.ID)
	if _, err := c.commands().tasksCmd(t.Context(), Request{
		Source: Source{ConversationID: root.Channel},
	}, "cancel "+root.ID); err != nil {
		t.Fatal(err)
	}
	if tracked, _ := c.tasks.Get(root.ID); tracked.State != task.StateCancelled {
		t.Fatal("cancel was not recorded")
	}
	if op, _ := readDiscardRun(t, book, runs[0].ID); op.State != exec.RunExecuting {
		t.Fatal("cancellation erased the retained run before discard")
	}
	if err := c.DiscardConversation(t.Context(), root.Channel); err != nil {
		t.Fatal(err)
	}
	for _, before := range runs {
		op, got := readDiscardRun(t, book, before.ID)
		if op.State != exec.RunCompleted || got.Phase != exec.RunCompleted || got.Outcome != "discarded" {
			t.Errorf("discard left nonterminal run: state=%s record=%+v", op.State, got)
		}
		if got.Error != before.Error || !reflect.DeepEqual(got.Sinks, before.Sinks) || !reflect.DeepEqual(got.Execution, before.Execution) {
			t.Error("discard erased failure, sinks or original authority")
		}
	}
	completedAfter, _ := readDiscardRun(t, book, completed.ID)
	if !reflect.DeepEqual(completedBefore, completedAfter) {
		t.Error("discard rewrote successful history")
	}
	for _, id := range []string{unrelated.ID, orphan.ID} {
		if op, _ := readDiscardRun(t, book, id); op.State != exec.RunExecuting {
			t.Error("discard changed unrelated or unknown history")
		}
	}
	for _, id := range []string{root.ID, child.ID, done.ID} {
		if _, found := c.tasks.Get(id); found {
			t.Error("discard kept task", id)
		}
	}
	if _, found := c.tasks.Get(other.ID); !found {
		t.Error("discard deleted another conversation's task")
	}
	if _, found, err := c.projects.Binding(t.Context(), root.Channel); err != nil || found {
		t.Errorf("discard kept binding: %t %v", found, err)
	}
	if _, found, err := c.projects.Binding(t.Context(), other.Channel); err != nil || !found {
		t.Errorf("discard lost unrelated binding: %t %v", found, err)
	}
	if _, found, err := book.Name(t.Context(), "conversation/"+root.Channel+"/project"); err != nil || found {
		t.Errorf("discard kept binding name: %t %v", found, err)
	}
	reopened, err := plan.OpenLedger(book)
	if err != nil || !sameAbandonJSON(history, reopened.Revisions(runs[0].PlanID)) {
		t.Errorf("plan revisions were erased: %v", err)
	}
	sup := exec.NewSupervisor(planner.Rule{}, exec.Deps{}, nil)
	sup.SetLedger(book, "next-process")
	open, err := sup.OpenRuns(t.Context())
	if err != nil || len(open) != 2 {
		t.Errorf("restart still discovers discarded runs: %+v %v", open, err)
	}
	if op, _ := readDiscardRun(t, book, runs[0].ID); op.State == exec.RunCompleted {
		if _, err := sup.Resume(t.Context(), runs[0]); err == nil {
			t.Error("discarded run replayed as success")
		}
	}
	if err := c.DiscardConversation(t.Context(), root.Channel); err != nil {
		t.Fatal("discard retry:", err)
	}
}

func TestDiscardConversationDropsBindingWithoutTasks(t *testing.T) {
	c := discardPlanCoordinator(t)
	if _, err := c.projects.Bind(t.Context(), "console:empty", "p", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := c.DiscardConversation(t.Context(), "console:empty"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.projects.Binding(t.Context(), "console:empty"); err != nil || found {
		t.Fatalf("empty conversation kept binding: %t %v", found, err)
	}
}

func TestDiscardConversationRollsBackPlanRetirementWithTaskDeletion(t *testing.T) {
	for _, point := range []string{"run", "task", "binding"} {
		t.Run(point, func(t *testing.T) {
			c := discardPlanCoordinator(t)
			tracked := discardPlanTask(t, c, "console:atomic", "")
			rec := discardPlanRun(t, c, tracked.ID, exec.RunExecuting)
			book := ledgerOf(t, c)
			if _, err := c.projects.Bind(t.Context(), tracked.Channel, "p", "fixture"); err != nil {
				t.Fatal(err)
			}
			sql := `CREATE TRIGGER cut BEFORE UPDATE ON operations WHEN NEW.kind='plan-run' AND NEW.state='completed' BEGIN SELECT RAISE(ABORT,'cut'); END`
			if point == "task" {
				sql = `CREATE TRIGGER cut BEFORE DELETE ON bindings WHEN OLD.kind='task' BEGIN SELECT RAISE(ABORT,'cut'); END`
			}
			if point == "binding" {
				sql = `CREATE TRIGGER cut BEFORE DELETE ON bindings WHEN OLD.kind='conversation-project' BEGIN SELECT RAISE(ABORT,'cut'); END`
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(sql); return err }); err != nil {
				t.Fatal(err)
			}
			if err := c.DiscardConversation(t.Context(), tracked.Channel); err == nil {
				t.Error("injected retirement failure was ignored")
			}
			op, _ := readDiscardRun(t, book, rec.ID)
			if op.State != exec.RunExecuting || op.Revision != 1 {
				t.Error("failed deletion committed plan retirement")
			}
			reopened, err := task.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			if _, found := reopened.Get(tracked.ID); !found {
				t.Error("failed deletion removed durable task authority")
			}
			if _, found := c.tasks.Get(tracked.ID); !found {
				t.Error("failed deletion removed cached task authority")
			}
			if _, found, err := c.projects.Binding(t.Context(), tracked.Channel); err != nil || !found {
				t.Errorf("failed deletion lost binding: %t %v", found, err)
			}
		})
	}
}

func TestDiscardConversationRefusesAPlanDriverBeforeDeletingTasks(t *testing.T) {
	c := discardPlanCoordinator(t)
	tracked := discardPlanTask(t, c, "console:driven", "")
	rec := discardPlanRun(t, c, tracked.ID, exec.RunExecuting)
	book := ledgerOf(t, c)
	lease, err := book.Acquire(t.Context(), "plan-driver:"+rec.ID, "still-driving", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DiscardConversation(t.Context(), tracked.Channel); !errors.Is(err, task.ErrRetirementPending) {
		t.Errorf("active driver was discarded: %v", err)
	}
	if op, _ := readDiscardRun(t, book, rec.ID); op.State != exec.RunExecuting {
		t.Error("active run was retired")
	}
	if _, found := c.tasks.Get(tracked.ID); !found {
		t.Error("active driver lost its task")
	}
	if err := book.Release(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if err := c.DiscardConversation(t.Context(), tracked.Channel); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardConversationKeepsPlanRunWhileNativeRetirementPending(t *testing.T) {
	c, r := abandonWithSession(t)
	rec := discardPlanRun(t, c, r.TaskID, exec.RunExecuting)
	if err := c.DiscardConversation(t.Context(), "console:original"); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("pending native cleanup was discarded: %v", err)
	}
	if op, _ := readDiscardRun(t, ledgerOf(t, c), rec.ID); op.State != exec.RunExecuting {
		t.Error("native stop owed was hidden by retirement")
	}
}

func TestDiscardConversationKeepsUnknownOwnedRunHistory(t *testing.T) {
	c := discardPlanCoordinator(t)
	tracked := discardPlanTask(t, c, "console:unknown", "")
	rec := discardPlanRun(t, c, tracked.ID, "future-phase")
	before, _ := readDiscardRun(t, ledgerOf(t, c), rec.ID)
	if err := c.DiscardConversation(t.Context(), tracked.Channel); err == nil {
		t.Fatal("unknown run lifecycle was guessed")
	}
	after, _ := readDiscardRun(t, ledgerOf(t, c), rec.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("unknown run history was rewritten")
	}
	if _, found := c.tasks.Get(tracked.ID); !found {
		t.Fatal("unknown run lost task authority")
	}
}

func TestDiscardConversationKeepsExpiredButRetainedPlanDriver(t *testing.T) {
	c := discardPlanCoordinator(t)
	tracked := discardPlanTask(t, c, "console:expired-driver", "")
	rec := discardPlanRun(t, c, tracked.ID, exec.RunExecuting)
	book := ledgerOf(t, c)
	lease, err := book.Acquire(t.Context(), "plan-driver:"+rec.ID, "unconfirmed-driver", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Check(t.Context(), lease); !errors.Is(err, ledger.ErrStale) {
		t.Fatalf("fixture lease not expired: %v", err)
	}
	if err := c.DiscardConversation(t.Context(), tracked.Channel); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("expiry mistaken for joined driver: %v", err)
	}
	if _, found := c.tasks.Get(tracked.ID); !found {
		t.Fatal("expired driver lost task authority")
	}
	if op, _ := readDiscardRun(t, book, rec.ID); op.State != exec.RunExecuting {
		t.Fatal("expired driver hid open run")
	}
}

// Completed responsibility is durable before the driver releases its lease;
// a crash at that boundary must not make a finished conversation undeletable.
func TestDiscardConversationKeepsCompletedRunDespiteRetainedDriver(t *testing.T) {
	c := discardPlanCoordinator(t)
	tracked := discardPlanTask(t, c, "console:completed-driver", "")
	rec := discardPlanRun(t, c, tracked.ID, exec.RunCompleted)
	book := ledgerOf(t, c)
	before, _ := readDiscardRun(t, book, rec.ID)
	if _, err := book.Acquire(t.Context(), "plan-driver:"+rec.ID, "completed-driver", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := c.DiscardConversation(t.Context(), tracked.Channel); err != nil {
		t.Fatalf("terminal run lease blocked deletion: %v", err)
	}
	after, _ := readDiscardRun(t, book, rec.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("completed run was changed")
	}
}

func TestDiscardConversationRefusesUnreadablePlanRunOwnership(t *testing.T) {
	c := discardPlanCoordinator(t)
	tracked := discardPlanTask(t, c, "console:corrupt-owner", "")
	rec := discardPlanRun(t, c, tracked.ID, exec.RunExecuting)
	book := ledgerOf(t, c)
	op, _ := readDiscardRun(t, book, rec.ID)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		if err := tx.SetData(&op, map[string]any{"id": rec.ID, "plan_id": rec.PlanID, "phase": rec.Phase, "task_id": 123}); err != nil {
			return err
		}
		return tx.RecordTransition(op, exec.RunExecuting, "corrupt-fixture")
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.DiscardConversation(t.Context(), tracked.Channel); err == nil {
		t.Fatal("unreadable run ownership was hidden by task selection")
	}
	if _, found := c.tasks.Get(tracked.ID); !found {
		t.Fatal("unreadable run lost its possible task authority")
	}
}

func TestDiscardConversationRefusesRunOwnedElsewhereButUsingItsToken(t *testing.T) {
	for _, route := range []string{"public-discard", "deletion-transaction"} {
		t.Run(route, func(t *testing.T) {
			c := discardPlanCoordinator(t)
			doomed := discardPlanTask(t, c, "console:owner-conflict", "")
			other := discardPlanTask(t, c, "console:other-owner", "")
			own := discardPlanRun(t, c, doomed.ID, exec.RunExecuting)
			conflict := discardPlanRun(t, c, other.ID, exec.RunExecuting)
			conflict.Execution = own.Execution
			book := ledgerOf(t, c)
			op, _ := readDiscardRun(t, book, conflict.ID)
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				if err := tx.SetData(&op, conflict); err != nil {
					return err
				}
				return tx.RecordTransition(op, exec.RunExecuting, "fixture-owner-conflict")
			}); err != nil {
				t.Fatal(err)
			}
			binding, err := c.projects.Bind(t.Context(), doomed.Channel, "p", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			before := c.tasks.List("")
			ownBefore, _ := readDiscardRun(t, book, own.ID)
			conflictBefore, _ := readDiscardRun(t, book, conflict.ID)
			if route == "public-discard" {
				err = c.DiscardConversation(t.Context(), doomed.Channel)
			} else {
				_, err = c.tasks.DeleteChannelWith(t.Context(), doomed.Channel, checkConversationRetirement, func(tx *ledger.Tx, ids []string) error {
					if err := exec.DiscardTaskRunsTx(tx, ids); err != nil {
						return err
					}
					return project.DiscardConversationBindingTx(tx, doomed.Channel)
				})
			}
			if err == nil {
				t.Error("cross-owner run escaped retirement admission")
			}
			if !reflect.DeepEqual(before, c.tasks.List("")) {
				t.Error("refused deletion changed cached tasks")
			}
			reopened, err := task.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			if _, found := reopened.Get(doomed.ID); !found {
				t.Error("conflicting token lost durable task authority")
			}
			if got, _ := readDiscardRun(t, book, own.ID); !reflect.DeepEqual(ownBefore, got) {
				t.Error("refused deletion retired a valid run")
			}
			if got, _ := readDiscardRun(t, book, conflict.ID); !reflect.DeepEqual(conflictBefore, got) {
				t.Error("refused deletion rewrote conflicting history")
			}
			if got, found, err := c.projects.Binding(t.Context(), doomed.Channel); err != nil || !found || !reflect.DeepEqual(binding, got) {
				t.Errorf("refused deletion lost binding: %+v %t %v", got, found, err)
			}
			if name, found, err := book.Name(t.Context(), "conversation/"+doomed.Channel+"/project"); err != nil || !found || name.Version != binding.Version {
				t.Errorf("refused deletion lost binding name: %+v %t %v", name, found, err)
			}
		})
	}
}

func TestDiscardConversationSelectsEveryPossibleRunOwnershipAnchor(t *testing.T) {
	for _, scenario := range []string{"selected-owner", "selected-token", "empty-owner", "selected-plan", "selected-operation-id", "foreign-plan", "sink-token", "unknown-token", "unrelated-valid"} {
		t.Run(scenario, func(t *testing.T) {
			c := discardPlanCoordinator(t)
			doomed := discardPlanTask(t, c, "console:ownership-anchors", "")
			other := discardPlanTask(t, c, "console:unrelated", "")
			own := discardPlanRun(t, c, doomed.ID, exec.RunExecuting)
			foreign := discardPlanRun(t, c, other.ID, exec.RunExecuting)
			target := foreign.ID
			switch scenario {
			case "selected-owner":
				foreign.TaskID = doomed.ID
			case "selected-token":
				foreign.Execution = own.Execution
			case "empty-owner":
				foreign.TaskID = ""
				foreign.Execution = own.Execution
			case "selected-plan":
				foreign.PlanID = own.PlanID
			case "selected-operation-id":
				target = own.ID
			case "foreign-plan":
				foreign.TaskID = doomed.ID
				foreign.Execution = own.Execution
			case "sink-token":
				foreign.Sinks = []exec.Sink{{StepID: "foreign", Source: &artifact.Source{Execution: own.Execution, AttemptID: "fixture"}}}
			case "unknown-token":
				foreign.Execution = &task.ExecutionToken{}
			}
			book := ledgerOf(t, c)
			op, _ := readDiscardRun(t, book, target)
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				if err := tx.SetData(&op, foreign); err != nil {
					return err
				}
				return tx.RecordTransition(op, exec.RunExecuting, "fixture-ownership-anchor")
			}); err != nil {
				t.Fatal(err)
			}
			before, _ := readDiscardRun(t, book, target)
			err := c.DiscardConversation(t.Context(), doomed.Channel)
			if scenario == "unrelated-valid" {
				if err != nil {
					t.Fatal("unrelated legal history permanently blocked discard", err)
				}
				if op, _ := readDiscardRun(t, book, own.ID); op.State != exec.RunCompleted {
					t.Fatal("owned run was not retired")
				}
			} else {
				if err == nil {
					t.Fatal("possible related owner conflict was missed", scenario)
				}
				if _, found := c.tasks.Get(doomed.ID); !found {
					t.Fatal("conflict deleted task authority")
				}
			}
			after, _ := readDiscardRun(t, book, target)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("ownership check rewrote history")
			}
		})
	}
}
