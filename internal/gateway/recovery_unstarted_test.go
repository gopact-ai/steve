package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

const unstartedFailure = "before-snapshot: content has insufficient durable replicas"

// unstartedProbe admits a turn and then fails it before a native session
// exists, without naming the attempt in its result, so the dispatch is
// recorded for recovery. Recovery is the coordinator's own: the probe hands
// the retained attempt to a real coordinator over the same ledger.
type unstartedProbe struct {
	calls, resumes atomic.Int32
	taskID         string
	coordinator    *turn.Coordinator
}

func (p *unstartedProbe) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	p.calls.Add(1)
	if req.OnTurnReady != nil {
		req.OnTurnReady(p.taskID, "original-attempt")
	}
	return turn.Result{}, errors.New(unstartedFailure)
}

func (p *unstartedProbe) ResumeRetainedChat(ctx context.Context, id string, req turn.Request) (turn.Result, error) {
	p.resumes.Add(1)
	return p.coordinator.ResumeRetainedChat(ctx, id, req)
}

type textChannel struct {
	mu    sync.Mutex
	texts []string
}

func (ch *textChannel) Reply(context.Context, string, string) error { return nil }
func (ch *textChannel) ReplyText(_ context.Context, _ string, text string) (string, error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.texts = append(ch.texts, text)
	return "reply-receipt", nil
}

func (ch *textChannel) sent() []string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return append([]string(nil), ch.texts...)
}

// unstartedFixture is the input's task with its accounting row bound to the
// original attempt, and the attempt as the given state and record left it.
// "{task}" in the record stands for the task's id.
func unstartedFixture(t *testing.T, attemptState, data string) (*ledger.Ledger, *task.Store, *unstartedProbe) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	msg := inboundFixture()
	tracked, err := tasks.Create(task.Task{Goal: "original work", Channel: conversationID(msg), Transport: "feishu", Member: "dev", Requester: msg.SenderOpenID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "dev", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, "original-attempt", msg.MessageID); err != nil {
		t.Fatal(err)
	}
	data = strings.ReplaceAll(data, "{task}", tracked.ID)
	if _, err := book.DB().Exec(`INSERT INTO operations(id,kind,state,revision,incarnation,data,created_at,updated_at)
		VALUES('original-attempt','attempt',?,2,1,?,'2026-09-20T21:55:53Z','2026-09-20T21:55:54Z')`, attemptState, data); err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"dev": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := turn.New(catalog, sessions, capability.NewAssembler(nil), nil, time.Minute)
	coordinator.SetTasks(tasks, "node")
	coordinator.SetAttempts(attempt.New(book))
	if err := coordinator.SetChannelOwner("feishu", msg.SenderOpenID); err != nil {
		t.Fatal(err)
	}
	return book, tasks, &unstartedProbe{taskID: tracked.ID, coordinator: coordinator}
}

func pendingInputs(t *testing.T, book *ledger.Ledger) int {
	t.Helper()
	pending, err := book.PendingCommands(t.Context(), gatewayInputKind)
	if err != nil {
		t.Fatal(err)
	}
	return len(pending)
}

// An input whose attempt failed before it ever held a native session has
// nothing on any node to resume. Its recorded failure is the outcome: the
// requester is told the turn failed, the task's row is settled at the
// attempt's end, and the input is acknowledged, instead of the input asking
// to resume a session that never existed on every recovery pass, forever.
func TestGatewayInputWhoseAttemptEndedWithoutANativeSessionDeliversItsFailure(t *testing.T) {
	book, tasks, p := unstartedFixture(t, "failed", `{"id":"original-attempt","task_id":"{task}","turn_id":"input-message","kind":"chat","agent":"dev","execution":{"task_id":"{task}","epoch":1},"session_settled":true,"error":"`+unstartedFailure+`","ended_at":"2026-09-20T21:55:54Z"}`)
	ch := &textChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if err := g.processAcceptedFixture(inboundFixture()); err != nil {
		t.Fatalf("an attempt that can never resume left its input pending: %v", err)
	}
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if pending := pendingInputs(t, book); pending != 0 {
		t.Fatalf("pending inputs = %d; want the input acknowledged", pending)
	}
	if sent := ch.sent(); len(sent) != 1 || strings.Contains(sent[0], "complete original result") {
		t.Fatalf("replies = %q; want exactly one failure reply", sent)
	}
	if p.calls.Load() != 1 || p.resumes.Load() != 1 {
		t.Fatalf("calls=%d resumes=%d; want the original dispatched once and recovered once", p.calls.Load(), p.resumes.Load())
	}
	tracked, _ := tasks.Get(p.taskID)
	if len(tracked.Attempts) != 1 || tracked.Attempts[0].Open() || tracked.Attempts[0].Outcome != task.OutcomeError ||
		!tracked.Attempts[0].EndedAt.Equal(time.Date(2026, 9, 20, 21, 55, 54, 0, time.UTC)) {
		t.Fatalf("accounting = %+v; want the row settled as an error at the attempt's end", tracked.Attempts)
	}
}

// Only an attempt that is over, settled, and never held a native session is
// known to have nothing retained. Anything else may still have work on a
// node, or does not belong to this input, and stays pending.
func TestGatewayInputKeepsRecoveringAnAttemptThatMayHoldWork(t *testing.T) {
	for name, fixture := range map[string]struct{ state, data string }{
		"running":                  {"running", `{"id":"original-attempt","task_id":"{task}","turn_id":"input-message","kind":"chat","agent":"dev","execution":{"task_id":"{task}","epoch":1}}`},
		"unsettled-stop":           {"failed", `{"id":"original-attempt","task_id":"{task}","turn_id":"input-message","kind":"chat","agent":"dev","execution":{"task_id":"{task}","epoch":1},"unsettled":true,"error":"x"}`},
		"unsettled-native-session": {"failed", `{"id":"original-attempt","task_id":"{task}","turn_id":"input-message","kind":"chat","agent":"dev","execution":{"task_id":"{task}","epoch":1},"session":"ns_1","unsettled":true,"error":"x"}`},
		"unproved-session":         {"failed", `{"id":"original-attempt","task_id":"{task}","turn_id":"input-message","kind":"chat","agent":"dev","execution":{"task_id":"{task}","epoch":1},"session":"pending-open","error":"x"}`},
		"another-task":             {"failed", `{"id":"original-attempt","task_id":"another-task","turn_id":"input-message","kind":"chat","agent":"dev","error":"x"}`},
		"another-message":          {"failed", `{"id":"original-attempt","task_id":"{task}","turn_id":"another-message","kind":"chat","agent":"dev","execution":{"task_id":"{task}","epoch":1},"error":"x"}`},
	} {
		t.Run(name, func(t *testing.T) {
			book, tasks, p := unstartedFixture(t, fixture.state, fixture.data)
			ch := &textChannel{}
			g := New(p)
			g.BindChannel(ch)
			g.SetRecoveryLedger(book)
			var blocked *agentexec.RecoveryBlocked
			if err := g.processAcceptedFixture(inboundFixture()); !errors.As(err, &blocked) {
				t.Fatalf("recovery outcome = %v; want the coordinator's block", err)
			}
			if pendingInputs(t, book) != 1 || len(ch.sent()) != 0 || p.resumes.Load() != 1 {
				t.Fatalf("pending=%d replies=%q resumes=%d; want one pending input and no reply", pendingInputs(t, book), ch.sent(), p.resumes.Load())
			}
			if tracked, _ := tasks.Get(p.taskID); len(tracked.Attempts) != 1 || !tracked.Attempts[0].Open() {
				t.Fatalf("accounting = %+v; want the row left open", tracked.Attempts)
			}
		})
	}
}

type pendingLogCounter struct {
	mu     sync.Mutex
	errors []string
}

func (h *pendingLogCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *pendingLogCounter) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *pendingLogCounter) WithGroup(string) slog.Handler            { return h }
func (h *pendingLogCounter) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "gateway recovery remains pending" {
		return nil
	}
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "error" {
			h.mu.Lock()
			h.errors = append(h.errors, attr.Value.String())
			h.mu.Unlock()
		}
		return true
	})
	return nil
}

func (h *pendingLogCounter) logged() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.errors...)
}

// changingProbe leaves its dispatch to recovery and then stays blocked on
// every resume, with a reason the test can change between passes.
type changingProbe struct {
	mu     sync.Mutex
	reason string
}

func (p *changingProbe) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	if req.OnTurnReady != nil {
		req.OnTurnReady("original-task", "original-attempt")
	}
	return turn.Result{}, errors.New(unstartedFailure)
}

func (p *changingProbe) ResumeRetainedChat(context.Context, string, turn.Request) (turn.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return turn.Result{}, errors.New(p.reason)
}

// Recovery retries a pending input on every pass. A reason that has not
// changed since the last pass is the same state, not a new event: it is
// logged when it first appears and again only when it changes.
func TestGatewayPendingRecoveryLogsOncePerReason(t *testing.T) {
	logs := &pendingLogCounter{}
	previous := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(previous) })
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &changingProbe{reason: "original node offline"}
	g := New(p)
	g.BindChannel(&textChannel{})
	g.SetRecoveryLedger(book)
	if err := g.processAcceptedFixture(inboundFixture()); err == nil {
		t.Fatal("expected the input to stay pending")
	}
	pass := func() {
		t.Helper()
		var workers recoveryTestWorkers
		if err := g.ReconcileQueued(t.Context(), book, p, nil, &workers); err != nil {
			t.Fatal(err)
		}
		workers.Wait()
	}
	for range 3 {
		pass()
	}
	if got := logs.logged(); len(got) != 1 {
		t.Fatalf("unchanged pending reason logged %d times: %q", len(got), got)
	}
	p.mu.Lock()
	p.reason = "original session rejected its receipt"
	p.mu.Unlock()
	for range 2 {
		pass()
	}
	if got := logs.logged(); len(got) != 2 || got[1] != "original session rejected its receipt" {
		t.Fatalf("changed pending reason logs = %q; want it reported once", got)
	}
}
