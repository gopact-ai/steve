package turn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

type retainedPlanSupervisor struct {
	planned, resumedPlanning, executed, resumed int
	attemptID                                   string
	proposed                                    plan.Plan
	runs                                        []exec.RunRecord
	execution                                   *task.ExecutionToken
	err                                         error
}

func (s *retainedPlanSupervisor) Plan(context.Context, planner.Request) (plan.Plan, error) {
	s.planned++
	return s.proposed, s.err
}
func (s *retainedPlanSupervisor) ResumePlanning(_ context.Context, id string) (plan.Plan, error) {
	s.resumedPlanning++
	s.attemptID = id
	return s.proposed, s.err
}
func (s *retainedPlanSupervisor) Execute(ctx context.Context, _ plan.Plan) (exec.Outcome, error) {
	s.executed++
	s.execution = execution.Token(ctx)
	return exec.Outcome{}, s.err
}
func (s *retainedPlanSupervisor) Resume(context.Context, exec.RunRecord) (exec.Outcome, error) {
	s.resumed++
	return exec.Outcome{}, s.err
}
func (s *retainedPlanSupervisor) Name() string                          { return "retained-test" }
func (s *retainedPlanSupervisor) PrepareRecovery(context.Context) error { return nil }
func (s *retainedPlanSupervisor) OpenRuns(context.Context) ([]exec.RunRecord, error) {
	return s.runs, nil
}

func retainedPlanFixture(t *testing.T) (*Coordinator, *retainedPlanSupervisor, RetainedPlan, Request) {
	t.Helper()
	c, _, book, _, _ := retainedChatFixture(t)
	tracked, err := c.tasks.Create(task.Task{Transport: "console", Origin: "plan", Goal: "original plan goal", ProjectID: "p", Channel: "console:plan", Requester: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.tasks.SetAnchor(tracked.ID, channel.Address{Channel: "console", Conversation: tracked.Channel, Message: "web-original-plan"}, "console", "p2p", ""); err != nil {
		t.Fatal(err)
	}
	token, err := c.tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "original-planning", TaskID: tracked.ID, TurnID: "plan/" + tracked.ID + "/r1/prompt/1", Project: "p", Kind: attempt.KindPlan, Node: "node-a", Harness: "test", Agent: "worker", Execution: &token, Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "planning-workspace", Project: "p", Path: t.TempDir(), Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []attempt.State{attempt.Prepared, attempt.Running} {
		if _, err := c.attempts.Advance(t.Context(), record.ID, state, "test", func(r *attempt.Record) { r.Session = "ns_planning" }); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := plan.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	sup := &retainedPlanSupervisor{proposed: plan.Plan{TaskID: tracked.ID, Goal: tracked.Goal, By: "llm:worker", Steps: []plan.Step{{ID: "work", Goal: "original step", Agent: "worker", Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "no side effects"}}}}}
	c.SetSupervisor(sup, plans, nil)
	identity := RetainedPlan{TaskID: tracked.ID, Conversation: tracked.Channel, MessageID: "web-original-plan", AttemptID: record.ID, ProjectID: "p"}
	return c, sup, identity, Request{Channel: "console", ConversationID: tracked.Channel, MessageID: identity.MessageID, SenderOpenID: "owner", ExpectedProject: "p"}
}

func TestRetainedPlanDiscoversInitialPlanningAndPreservesOriginalTask(t *testing.T) {
	c, sup, identity, req := retainedPlanFixture(t)
	items, err := c.RetainedPlans(t.Context())
	if err != nil || len(items) != 1 || items[0].PlanID != "" || items[0].RunID != "" || items[0].MessageID != identity.MessageID || items[0].AttemptID != identity.AttemptID {
		t.Fatalf("initial planning was not discoverable: %+v %v", items, err)
	}
	before := len(c.tasks.List(""))
	result, err := c.ResumeRetainedPlan(t.Context(), items[0], req)
	if err != nil || !strings.Contains(result.Text, "original step") {
		t.Fatalf("resume result=%+v err=%v", result, err)
	}
	stored, ok := c.plans.ForTask(identity.TaskID)
	if !ok || sup.planned != 0 || sup.resumedPlanning != 1 || sup.executed != 1 || sup.attemptID != identity.AttemptID || len(c.tasks.List("")) != before || sup.execution == nil || stored.Execution == nil || *sup.execution != *stored.Execution {
		t.Fatalf("planning replayed or lost original ownership: stored=%+v sup=%+v", stored, sup)
	}
	// A crash after plan creation reuses the stored plan. It must not ask the
	// planning agent again or create a second plan for the same exchange.
	if _, err := c.ResumeRetainedPlan(t.Context(), items[0], req); err != nil {
		t.Fatal(err)
	}
	if len(c.plans.List()) != 1 || sup.resumedPlanning != 1 || sup.executed != 2 {
		t.Fatal("the stored plan was duplicated")
	}
}

func TestRetainedPlanningCannotAdoptResumedTaskEpoch(t *testing.T) {
	c, sup, identity, req := retainedPlanFixture(t)
	if _, err := c.tasks.SetAside(identity.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.Advance(identity.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	_, err := c.ResumeRetainedPlan(t.Context(), identity, req)
	var blocked *agentexec.RecoveryBlocked
	if !errors.As(err, &blocked) || sup.resumedPlanning != 0 || sup.executed != 0 || len(c.plans.List()) != 0 {
		t.Fatalf("old planning used resumed task authority: err=%v sup=%+v", err, sup)
	}
}

func TestRetainedPlanKeepsRecoveryQuestionInOriginalExchange(t *testing.T) {
	c, sup, identity, req := retainedPlanFixture(t)
	sup.err = agentexec.Blocked(attempt.Record{Spec: attempt.Spec{ID: identity.AttemptID, TaskID: identity.TaskID}}, "offline", agentexec.Diagnosis{Attempted: i18n.ExecTriedReachStepNode, Problem: i18n.ExecProblemStepUnsafe, Recommendation: i18n.ExecAdviceRestoreNode}, errors.New("node unreachable"))
	result, err := c.ResumeRetainedPlan(t.Context(), identity, req)
	var blocked *agentexec.RecoveryBlocked
	if !errors.As(err, &blocked) || result.Text != "" || !strings.Contains(blocked.Question.Message, "node unreachable") || sup.planned != 0 || sup.executed != 0 {
		t.Fatalf("retained planning was reported as terminal: result=%+v err=%v", result, err)
	}
}

func TestRetainedPlanRejectsDifferentExchangeBeforeExecution(t *testing.T) {
	c, sup, identity, req := retainedPlanFixture(t)
	req.MessageID = "web-another-exchange"
	if _, err := c.ResumeRetainedPlan(t.Context(), identity, req); err == nil || sup.resumedPlanning != 0 || sup.executed != 0 {
		t.Fatalf("another exchange consumed the plan: %v", err)
	}
}

func TestRulePlanStoreFailureRecoversFrozenPlanInOriginalTask(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "revoked"}[resumed], func(t *testing.T) {
			c, _, book, _, _ := retainedChatFixture(t)
			plans, err := plan.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			rule := planner.Rule{Workflows: map[string][]plan.Step{"release": {{ID: "original-rule-step", Goal: "original declared workflow", Agent: "worker", Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "no side effects"}}}}}
			c.SetSupervisor(exec.NewSupervisor(rule, exec.Deps{}, nil), plans, nil)
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER reject_plan BEFORE INSERT ON bindings WHEN NEW.kind='document' AND NEW.id='plans' BEGIN SELECT RAISE(FAIL,'plan store unavailable'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			req := Request{Channel: "console", ConversationID: "console:rule", MessageID: "web-rule-original", ChatID: "console", SenderOpenID: "owner", ExpectedProject: "p"}
			_, err = c.commands().planCmd(t.Context(), req, "release original goal")
			var blocked *agentexec.RecoveryBlocked
			if !errors.As(err, &blocked) {
				t.Fatalf("plan store failure did not preserve recovery: %v", err)
			}
			// Replace the planner configuration before recovery: the frozen
			// pure result must not invoke this new planning configuration.
			changed := &retainedPlanSupervisor{proposed: plan.Plan{Goal: "changed config"}}
			c.SetSupervisor(changed, plans, nil)
			items, err := c.RetainedPlans(t.Context())
			if err != nil || len(items) != 1 || items[0].AttemptID != "" || items[0].PlanID != "" || items[0].MessageID != req.MessageID {
				t.Fatalf("pure planned task disappeared: %+v %v", items, err)
			}
			before := len(c.tasks.List(""))
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER reject_plan"); return err }); err != nil {
				t.Fatal(err)
			}
			if resumed {
				if _, err := c.tasks.SetAside(items[0].TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
				if _, err := c.tasks.Advance(items[0].TaskID, task.StateRunning); err != nil {
					t.Fatal(err)
				}
			}
			result, err := c.ResumeRetainedPlan(t.Context(), items[0], req)
			if resumed {
				if !errors.As(err, &blocked) || changed.executed != 0 || len(plans.List()) != 0 {
					t.Fatalf("old pure plan adopted resumed authorization: %+v %v", result, err)
				}
			} else if err != nil || changed.executed != 1 || changed.planned != 0 || changed.resumedPlanning != 0 || len(c.tasks.List("")) != before || !strings.Contains(result.Text, "original declared workflow") {
				t.Fatalf("pure planning was lost, regenerated or duplicated: result=%+v sup=%+v err=%v", result, changed, err)
			}
		})
	}
}

// A question the execution layer raised for an attempt it could not settle
// reaches the exchange marked stop-unconfirmed; one the coordinator raised
// is already the exchange's own and passes through unchanged.
func TestPlanRecoveryErrorMarksOnlyExecutionQuestionsStopUnconfirmed(t *testing.T) {
	c, _, _, _, _ := retainedChatFixture(t)
	execution := agentexec.Blocked(attempt.Record{Spec: attempt.Spec{ID: "att-1", TaskID: "task-1"}}, "offline", agentexec.Diagnosis{Attempted: i18n.ExecTriedReachStepNode, Problem: i18n.ExecProblemStepUnsafe, Recommendation: i18n.ExecAdviceRestoreNode}, nil)
	var surfaced *agentexec.RecoveryBlocked
	err := c.planRecoveryError(fmt.Errorf("step: %w", execution))
	if !errors.As(err, &surfaced) || !errors.Is(err, harness.ErrStopUnconfirmed) || surfaced.AttemptID != "" || surfaced.TaskID != "" || surfaced.Question.RequestID != execution.Question.RequestID {
		t.Fatalf("execution question surfaced as %#v (%v)", surfaced, err)
	}
	if again := c.planRecoveryError(err); again != err {
		t.Fatalf("a surfaced question was wrapped again: %v", again)
	}
	own := c.retainedBlocked("plan-conditions", "检查", "问题", "原因", "建议", errors.New("no machine"))
	if got := c.planRecoveryError(fmt.Errorf("plan: %w", own)); got != own || errors.Is(got, harness.ErrStopUnconfirmed) {
		t.Fatalf("coordinator question changed: %v", got)
	}
}

// The execution layer asks in English; the question reaches the exchange in
// the exchange's language.
func TestPlanRecoveryErrorAsksExecutionQuestionsInTheExchangeLanguage(t *testing.T) {
	c, _, _, _, _ := retainedChatFixture(t)
	record := attempt.Record{Spec: attempt.Spec{ID: "att-1", TaskID: "task-1"}}
	diagnosis := agentexec.Diagnosis{Attempted: i18n.ExecTriedReachStepNode, Problem: i18n.ExecProblemStepUnsafe, Recommendation: i18n.ExecAdviceRestoreNode}
	execution := agentexec.Blocked(record, "offline", diagnosis, errors.New("node unreachable"))
	for _, locale := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		var surfaced *agentexec.RecoveryBlocked
		err := c.localized(locale).planRecoveryError(fmt.Errorf("step: %w", execution))
		want := agentexec.BlockedIn(i18n.New(locale), record, "offline", diagnosis, execution.Cause).Question
		if !errors.As(err, &surfaced) || surfaced.Question.Title != want.Title || surfaced.Question.Message != want.Message || surfaced.Question.RequestID != want.RequestID {
			t.Fatalf("%s question = %+v, want %+v", locale, surfaced, want)
		}
		if surfaced.AttemptID != "" || surfaced.TaskID != "" || !errors.Is(err, harness.ErrStopUnconfirmed) {
			t.Fatalf("%s question still names its attempt or lost its stop: %#v", locale, surfaced)
		}
	}
}

func TestRetainedBlockedAsksInTheCoordinatorLanguage(t *testing.T) {
	c, _, _, _, _ := retainedChatFixture(t)
	q := c.localized(i18n.LocaleEN).retainedBlocked("offline", "检查原节点", "问题", "原因", "建议", nil).Question
	if q.RequestID != "recovery/offline" || q.Title != "Continuing this task needs your decision" || !strings.HasPrefix(q.Message, "Tried: 检查原节点\n\n") {
		t.Fatalf("question = %+v", q)
	}
	if len(q.Choices) != 2 || q.Choices[0].Label != "Recheck the original execution" || q.Choices[1].Label != "Wait for now" {
		t.Fatalf("choices = %+v", q.Choices)
	}
	zh := c.localized(i18n.LocaleZH).retainedBlocked("offline", "检查原节点", "问题", "原因", "建议", nil).Question
	if zh.Title != "继续任务需要你的处理" || zh.Message != "已尝试：检查原节点\n\n问题\n\n原因\n\n建议" || zh.Choices[1].Label != "暂时等待" {
		t.Fatalf("zh question = %+v", zh)
	}
}
