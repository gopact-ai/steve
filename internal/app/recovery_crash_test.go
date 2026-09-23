package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

const crashConversation = "console:crash"
const crashAnswer = "complete durable answer, not another native prompt"

type crashProbe struct {
	book     *ledger.Ledger
	tasks    *task.Store
	attempts *attempt.Service
	c        *turn.Coordinator
	cons     *console.Service
	ctx      context.Context
	cancel   context.CancelFunc
	calls    atomic.Int32
	// worked is how long the seeded execution runs before its session is
	// confirmed settled: real work the dead process did before it stopped.
	worked time.Duration
}

// The native execution has ended before these windows. Only an isolated
// admission probe replaces Handle; persistence and recovery use real owners.
func (f *crashProbe) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	f.calls.Add(1)
	_, err := f.tasks.BeginTurn(req.ExpectedTask, "worker", "node-a", task.TurnInput{
		Address: req.Address(), ChatID: req.ChatID, ChatType: string(req.ChatType), Continuation: true,
	})
	if err != nil {
		return turn.Result{}, err
	}
	_, err = f.tasks.Finish(req.ExpectedTask, task.OutcomeOK, task.Tokens{}, 0)
	return turn.Result{Text: "unexpected new execution", AgentID: "worker"}, err
}

func openCrashProbe(t *testing.T, dir string) *crashProbe {
	t.Helper()
	f := &crashProbe{}
	var err error
	f.book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.tasks, err = task.OpenLedger(f.book, "")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(f.book, "")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "test", Node: "node-a", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.attempts = attempt.New(f.book)
	f.c = turn.New(catalog, sessions, capability.NewAssembler(nil), manager, time.Minute)
	f.c.SetTasks(f.tasks, "hub")
	f.c.SetAttempts(f.attempts)
	if err := f.c.SetChannelOwner("feishu", "owner"); err != nil {
		t.Fatal(err)
	}
	f.ctx, f.cancel = context.WithCancel(t.Context())
	f.cons = console.New(f, "owner", nil)
	f.cons.EnableRetainedRecovery(f.ctx)
	return f
}

func (f *crashProbe) close(t *testing.T) {
	t.Helper()
	f.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.cons.Shutdown(ctx); err != nil {
		t.Error(err)
	}
	if err := f.book.Close(); err != nil {
		t.Error(err)
	}
}

func (f *crashProbe) seed(t *testing.T, bound bool, exchangeState consoleapi.ExchangeState, transports ...string) attempt.Record {
	t.Helper()
	transport := "console"
	if len(transports) > 0 {
		transport = transports[0]
	}
	tracked, err := f.tasks.Create(task.Task{Transport: transport, Channel: crashConversation, Member: "worker", Requester: "owner", Goal: "original goal"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.tasks.BeginTurn(tracked.ID, "worker", "node-a", task.TurnInput{Address: channel.Address{Channel: transport, Conversation: crashConversation, Message: "web-original"}, ChatID: "console", ChatType: "p2p"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.attempts.Open(f.ctx, attempt.Spec{ID: "original-attempt", TaskID: tracked.ID, TurnID: "web-original", Kind: attempt.KindChat, Agent: "worker", Node: "node-a", Harness: "test", Project: "p", Workspace: project.Workspace{ID: "w", Project: "p", Node: "node-a", Path: t.TempDir(), Kind: project.KindCanonical}, Scope: attempt.ScopeUnrestricted, Execution: &token})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	session := "local-original"
	if bound {
		session = "ns_original"
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = f.attempts.Advance(f.ctx, r.ID, phase, "test", func(r *attempt.Record) { r.Session = session })
		if err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(f.worked)
	if err := f.attempts.MarkSessionSettled(f.ctx, r.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if bound {
		if _, err := f.attempts.Advance(f.ctx, r.ID, attempt.BindReady, "test", nil); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(turn.Result{Text: crashAnswer, AgentID: "worker", Attempt: r.ID})
		if err != nil {
			t.Fatal(err)
		}
		r, err = f.attempts.Complete(f.ctx, r.ID, "test", attempt.Completion{Result: attempt.Result{Output: raw}, Usage: &attempt.Usage{Input: 7, Output: 11, Model: "original-model", Reported: true}})
		if err != nil {
			t.Fatal(err)
		}
	} else {
		r, err = f.attempts.Get(f.ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	exchanges := map[string][]console.DurableExchange{}
	replies := map[string][]consoleapi.Reply{}
	if exchangeState != "" {
		e := consoleapi.Exchange{ID: "original", Conversation: crashConversation, Input: "original goal", Requester: "owner", State: exchangeState, EnqueuedAt: time.Now(), StartedAt: time.Now()}
		if exchangeState.Terminal() {
			e.ReplyID = "original-reply"
			replies[crashConversation] = []consoleapi.Reply{{ID: e.ReplyID, ExchangeID: e.ID, Conversation: crashConversation, Kind: "reply", Text: crashAnswer}}
		}
		exchanges[crashConversation] = []console.DurableExchange{{Exchange: e}}
	}
	if err := f.book.Update(f.ctx, func(tx *ledger.Tx) error {
		return console.StoreStateTx(tx, console.DurableState{Replies: replies, Exchanges: exchanges})
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *crashProbe) assemble(t *testing.T) error {
	t.Helper()
	report, err := f.attempts.PrepareRecovery(f.ctx, "startup")
	if err != nil {
		return err
	}
	quarantined := map[string]bool{}
	for _, r := range report.Quarantined {
		quarantined[r.TaskID] = true
	}
	return assembleRecovery(&assemblyInput{environment: &Environment{}}, &runtimeValues{book: f.book, ctx: f.ctx}, &ledgerValues{attempts: f.attempts, quarantinedTasks: quarantined}, &executionValues{coordinator: f.c, tasks: f.tasks, catalogText: i18n.New(i18n.LocaleEN)}, &consoleValues{cons: f.cons})
}

func (f *crashProbe) waitTerminal(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ready := true
		for _, e := range f.cons.Queue(crashConversation) {
			if !e.State.Terminal() {
				ready = false
			}
		}
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("recovery did not settle: %+v", f.cons.Queue(crashConversation))
}

func rejectCrashAccounting(t *testing.T, f *crashProbe) {
	t.Helper()
	_, err := f.book.DB().Exec(`CREATE TRIGGER reject_crash_accounting BEFORE INSERT ON bindings WHEN NEW.kind='task-attempt' AND json_extract(NEW.data,'$.usage_known') IS NOT NULL BEGIN SELECT RAISE(ABORT,'injected accounting crash'); END`)
	if err != nil {
		t.Fatal(err)
	}
}
func dropCrashAccounting(t *testing.T, f *crashProbe) {
	t.Helper()
	if _, err := f.book.DB().Exec(`DROP TRIGGER reject_crash_accounting`); err != nil {
		t.Fatal(err)
	}
}
func crashRequest(r attempt.Record) turn.Request {
	return turn.Request{Channel: "console", ConversationID: crashConversation, MessageID: r.TurnID, SenderOpenID: "owner"}
}

func TestStartupBoundCompletionDoesNotCreateContinuation(t *testing.T) {
	for _, exchangeState := range []consoleapi.ExchangeState{consoleapi.ExchangeRunning, consoleapi.ExchangeDone} {
		t.Run(string(exchangeState), func(t *testing.T) {
			dir := t.TempDir()
			first := openCrashProbe(t, dir)
			r := first.seed(t, true, exchangeState)
			rejectCrashAccounting(t, first)
			_, err := first.c.ResumeRetainedChat(first.ctx, r.ID, crashRequest(r))
			if err == nil || !strings.Contains(err.Error(), "injected accounting crash") {
				t.Fatalf("fault did not reach real accounting boundary: %v", err)
			}
			first.close(t)
			recovered := openCrashProbe(t, dir)
			defer recovered.close(t)
			dropCrashAccounting(t, recovered)
			if err := recovered.assemble(t); err != nil {
				t.Fatal(err)
			}
			recovered.waitTerminal(t)
			tracked, _ := recovered.tasks.Get(r.TaskID)
			t.Logf("durable Bound recovery: exchanges=%d new_handle=%d turns=%d tokens=%d original_outcome=%s", len(recovered.cons.Queue(crashConversation)), recovered.calls.Load(), tracked.Budget.Turns, tracked.Budget.Tokens.Total, tracked.Attempts[0].Outcome)
			if recovered.calls.Load() != 0 || len(recovered.cons.Queue(crashConversation)) != 1 || tracked.Budget.Turns != 1 {
				t.Errorf("committed completion created a new continuation/charge")
			}
			if tracked.Budget.Tokens.Total != 18 || tracked.Attempts[0].Outcome != task.OutcomeOK {
				t.Errorf("original durable completion was not projected exactly: %+v", tracked.Attempts[0])
			}
		})
	}
}

type crashGatewayChannel struct{ replies atomic.Int32 }

func (*crashGatewayChannel) Reply(context.Context, string, string) error {
	return errors.New("unexpected non-receipted reply")
}
func (ch *crashGatewayChannel) ReplyText(_ context.Context, anchor, text string) (string, error) {
	if anchor != "web-original" || text != crashAnswer {
		return "", errors.New("recovery changed the original anchor or complete result")
	}
	ch.replies.Add(1)
	return "durable-reply", nil
}

func TestStartupFeishuCompletionKeepsAcceptedRecoveryOwnerAcrossAccountingCrash(t *testing.T) {
	dir := t.TempDir()
	first := openCrashProbe(t, dir)
	r := first.seed(t, true, consoleapi.ExchangeDone, "feishu")
	gw := gateway.New(first)
	key := "original-recovery"
	if err := gw.QueueRecovery(first.ctx, first.book, key, gateway.Revival{
		TaskID: r.TaskID, Goal: "original goal", Member: "worker", ConversationID: crashConversation,
		MessageID: "before-resume", Requester: "owner", ChatID: "console", ChatType: "p2p",
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := first.book.RecordCommand(first.ctx, key+"/notice", "gateway-recovery-notice", "owner", json.RawMessage(`"web-original"`)); err != nil {
		t.Fatal(err)
	}
	receipt, err := json.Marshal(map[string]string{
		"input_id": key, "task_id": r.TaskID, "attempt_id": r.ID, "conversation": crashConversation, "message_id": "web-original",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.book.RecordCommand(first.ctx, key+"/attempt", "gateway-input-attempt", "owner", receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := first.book.DB().Exec(`CREATE TRIGGER refuse_dispatch_completion BEFORE UPDATE ON commands
		WHEN NEW.kind='gateway-recovery-dispatch' BEGIN SELECT RAISE(ABORT,'dispatch result lost'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.book.Command(first.ctx, key+"/dispatch", "gateway-recovery-dispatch", "owner",
		func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }); err == nil {
		t.Fatal("dispatch result write fault was not injected")
	}
	rejectCrashAccounting(t, first)
	req := crashRequest(r)
	req.Channel = "feishu"
	if _, err := first.c.ResumeRetainedChat(first.ctx, r.ID, req); err == nil || !strings.Contains(err.Error(), "injected accounting crash") {
		t.Fatalf("accounting fault was not injected: %v", err)
	}
	first.close(t)

	recovered := openCrashProbe(t, dir)
	defer recovered.close(t)
	dropCrashAccounting(t, recovered)
	if err := recovered.cons.PersistLedger(recovered.book); err != nil {
		t.Fatal(err)
	}
	ch := &crashGatewayChannel{}
	gw = gateway.New(recovered)
	gw.BindChannel(ch)
	gw.SetRecoveryLedger(recovered.book)
	recovery := newApplicationRecovery(recovered.book, recovered.attempts, recovered.tasks, recovered.c, gw, recovered.cons, i18n.New(i18n.LocaleEN), false)
	defer recovery.workers.Close()
	for range 2 {
		if err := recovery.Reconcile(recovered.ctx); err != nil {
			t.Fatal(err)
		}
		recovery.workers.group.Wait()
	}
	if _, exists, err := recovered.book.CommandReceipt(recovered.ctx, "task-result/"+r.ID); err != nil || exists {
		t.Fatalf("original result acquired a second delivery intent: exists=%v err=%v", exists, err)
	}
	tracked, _ := recovered.tasks.Get(r.TaskID)
	if ch.replies.Load() != 1 || recovered.calls.Load() != 0 || tracked.Budget.Turns != 1 ||
		tracked.Budget.Tokens.Total != 18 || tracked.Attempts[0].Open() {
		t.Fatalf("completion recovery duplicated/lost work: replies=%d new_handle=%d task=%+v", ch.replies.Load(), recovered.calls.Load(), tracked)
	}
}

func TestStartupResumeIntentSurvivesEnqueueFailure(t *testing.T) {
	dir := t.TempDir()
	first := openCrashProbe(t, dir)
	r := first.seed(t, false, "")
	first.close(t)
	failed := openCrashProbe(t, dir)
	// Startup may persist the loaded empty queue. Only accepting the continuation
	// fails, after task Finish has committed; no process or network is involved.
	_, err := failed.book.DB().Exec(`CREATE TRIGGER reject_crash_continuation BEFORE INSERT ON bindings WHEN NEW.kind='console-exchange' AND NEW.data LIKE '%"expected_task"%' BEGIN SELECT RAISE(ABORT,'injected continuation enqueue crash'); END`)
	if err != nil {
		t.Fatal(err)
	}
	assembleErr := failed.assemble(t)
	tracked, _ := failed.tasks.Get(r.TaskID)
	terminal, err := failed.attempts.Get(failed.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State != attempt.Expired || terminal.SessionSettled == nil || !*terminal.SessionSettled {
		t.Fatalf("missing durable expiry/settled evidence: %+v", terminal)
	}
	if assembleErr == nil || !tracked.Attempts[0].Open() || len(failed.cons.Queue(crashConversation)) != 0 {
		t.Errorf("enqueue failure consumed the only durable recovery intent: err=%v row=%+v queue=%+v", assembleErr, tracked.Attempts[0], failed.cons.Queue(crashConversation))
	}
	t.Logf("after failed enqueue: startup_err=%v task_open=%v queue=%d", assembleErr, tracked.Attempts[0].Open(), len(failed.cons.Queue(crashConversation)))
	if _, err := failed.book.DB().Exec(`DROP TRIGGER reject_crash_continuation`); err != nil {
		t.Fatal(err)
	}
	failed.close(t)
	retried := openCrashProbe(t, dir)
	defer retried.close(t)
	if err := retried.assemble(t); err != nil {
		t.Fatal(err)
	}
	retried.waitTerminal(t)
	if retried.calls.Load() != 1 {
		t.Errorf("durable continuation intent lost across reopen: handle=%d interrupted=%d queue=%+v", retried.calls.Load(), len(retried.tasks.Interrupted()), retried.cons.Queue(crashConversation))
	}
}

func TestRetainedCompletionAccountingRetryAfterCrash(t *testing.T) {
	dir := t.TempDir()
	first := openCrashProbe(t, dir)
	r := first.seed(t, true, consoleapi.ExchangeRunning)
	first.close(t)
	failed := openCrashProbe(t, dir)
	rejectCrashAccounting(t, failed)
	result, err := failed.c.ResumeRetainedChat(failed.ctx, r.ID, crashRequest(r))
	if err == nil || result.Attempt != "" {
		t.Fatalf("uncommitted accounting was delivered: %+v %v", result, err)
	}
	tracked, _ := failed.tasks.Get(r.TaskID)
	if !tracked.Attempts[0].Open() || tracked.Budget.Tokens.Total != 0 {
		t.Fatal("failed accounting was not atomic")
	}
	dropCrashAccounting(t, failed)
	failed.close(t)
	retried := openCrashProbe(t, dir)
	defer retried.close(t)
	for range 2 {
		result, err = retried.c.ResumeRetainedChat(retried.ctx, r.ID, crashRequest(r))
		if err != nil || result.Text != crashAnswer || result.Attempt != r.ID {
			t.Fatalf("bound delivery requires no native runtime: %+v %v", result, err)
		}
	}
	tracked, _ = retried.tasks.Get(r.TaskID)
	if tracked.Budget.Turns != 1 || tracked.Budget.Tokens.Total != 18 || tracked.Attempts[0].Open() || tracked.Attempts[0].Outcome != task.OutcomeOK {
		t.Fatalf("duplicate or missing accounting: %+v", tracked)
	}
	if retried.calls.Load() != 0 {
		t.Fatal(errors.New("replayed native input"))
	}
}

func TestStartupAcceptedContinuationCannotRunBeforeAccountingRetry(t *testing.T) {
	dir := t.TempDir()
	first := openCrashProbe(t, dir)
	original := first.seed(t, false, "")
	first.close(t)
	failed := openCrashProbe(t, dir)
	rejectCrashAccounting(t, failed)
	if err := failed.assemble(t); err == nil || !strings.Contains(err.Error(), "injected accounting crash") {
		t.Fatalf("fault missed pending accounting: %v", err)
	}
	saved, err := console.LoadState(failed.book)
	if err != nil {
		t.Fatal(err)
	}
	list := saved.Exchanges[crashConversation]
	if len(list) != 1 || !list[0].RecoveryPending || list[0].State != consoleapi.ExchangeQueued || failed.calls.Load() != 0 {
		t.Fatalf("accepted recovery started unaccounted work: queue=%+v calls=%d", list, failed.calls.Load())
	}
	id := list[0].ID
	failed.close(t)
	retried := openCrashProbe(t, dir)
	defer retried.close(t)
	dropCrashAccounting(t, retried)
	if err := retried.assemble(t); err != nil {
		t.Fatal(err)
	}
	retried.waitTerminal(t)
	got := retried.cons.Queue(crashConversation)
	tracked, _ := retried.tasks.Get(original.TaskID)
	if len(got) != 1 || got[0].ID != id || retried.calls.Load() != 1 || tracked.Budget.Turns != 2 {
		t.Fatalf("retry duplicated accepted continuation: queue=%+v calls=%d turns=%d", got, retried.calls.Load(), tracked.Budget.Turns)
	}
}

func TestRunningCompletionReconcilerRetriesAccountingWithoutRestart(t *testing.T) {
	f := openCrashProbe(t, t.TempDir())
	defer f.close(t)
	original := f.seed(t, true, consoleapi.ExchangeDone)
	if err := f.cons.PersistLedger(f.book); err != nil {
		t.Fatal(err)
	}
	recovery := newApplicationRecovery(f.book, f.attempts, f.tasks, f.c, nil, f.cons, i18n.New(i18n.LocaleEN), true)
	rejectCrashAccounting(t, f)
	if err := recovery.Reconcile(f.ctx); err == nil {
		t.Fatal("accounting write failure hidden")
	}
	dropCrashAccounting(t, f)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); runReconciler(ctx, "test completion accounting", recovery.Reconcile) }()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		tracked, _ := f.tasks.Get(original.TaskID)
		if !tracked.Attempts[0].Open() {
			if tracked.Budget.Tokens.Total != 18 || tracked.Budget.Turns != 1 || f.calls.Load() != 0 {
				t.Fatal("runtime recovery replayed work or accounting")
			}
			cancel()
			<-done
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("running reconciler never retried accounting")
}

func TestCompletionAccountingSurvivesLaterPauseOrCancellationEpoch(t *testing.T) {
	for _, stopped := range []task.State{task.StatePaused, task.StateCancelled} {
		t.Run(string(stopped), func(t *testing.T) {
			f := openCrashProbe(t, t.TempDir())
			defer f.close(t)
			r := f.seed(t, true, consoleapi.ExchangeDone)
			if _, err := f.tasks.SetAside(r.TaskID, stopped); err != nil {
				t.Fatal(err)
			}
			before, _ := f.tasks.Get(r.TaskID)
			if err := f.cons.PersistLedger(f.book); err != nil {
				t.Fatal(err)
			}
			recovery := newApplicationRecovery(f.book, f.attempts, f.tasks, f.c, nil, f.cons, i18n.New(i18n.LocaleEN), false)
			for range 2 {
				if err := recovery.Reconcile(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			after, _ := f.tasks.Get(r.TaskID)
			if after.State != stopped || after.ExecutionEpoch != before.ExecutionEpoch ||
				after.Attempts[0].Open() || after.Budget.Tokens.Total != 18 ||
				after.Budget.Turns != 1 || f.calls.Load() != 0 {
				t.Fatalf("durable original completion lost behind newer stop epoch: before=%+v after=%+v calls=%d", before, after, f.calls.Load())
			}
		})
	}
}

func TestStartupConsumedResumeWithoutAttemptCannotCreateUnscopedContinuation(t *testing.T) {
	dir := t.TempDir()
	first := openCrashProbe(t, dir)
	tracked, err := first.tasks.Create(task.Task{Transport: "console", Channel: crashConversation, Member: "worker", Requester: "owner", State: task.StatePaused})
	if err != nil {
		t.Fatal(err)
	}
	a := task.ResumeAdmission{ID: "manual-resume", TaskID: tracked.ID, Epoch: tracked.ExecutionEpoch + 1}
	if _, err := first.tasks.Resume(tracked.ID, tracked.ExecutionEpoch, tracked.State, a); err != nil {
		t.Fatal(err)
	}
	if _, err := first.tasks.BeginTurn(tracked.ID, "worker", "node-a", task.TurnInput{
		Address:      channel.Address{Channel: "console", Conversation: crashConversation, Message: "web-resume"},
		Continuation: true, ResumeAdmission: a, TurnID: "web-resume",
	}); err != nil {
		t.Fatal(err)
	}
	first.close(t)
	second := openCrashProbe(t, dir)
	defer second.close(t)
	err = second.assemble(t)
	if !errors.Is(err, task.ErrResumeConsumed) {
		t.Fatalf("missing attempt was treated as new execution permission: %v", err)
	}
	if got, _ := second.tasks.Get(tracked.ID); got.Budget.Turns != 1 || !got.HasOpenExecution() ||
		len(second.cons.Queue(crashConversation)) != 0 || second.calls.Load() != 0 {
		t.Fatalf("consumed manual admission replayed or lost: task=%+v queue=%+v calls=%d", got, second.cons.Queue(crashConversation), second.calls.Load())
	}
}
