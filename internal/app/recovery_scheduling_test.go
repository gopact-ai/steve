package app

import (
	"context"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"strings"
	"testing"
	"time"
)

func TestStartupSteerCannotConsumeDeferredAccountingContinuation(t *testing.T) {
	f := openCrashProbe(t, t.TempDir())
	defer f.close(t)
	original := f.seed(t, false, "")
	if _, err := f.attempts.PrepareRecovery(f.ctx, "review"); err != nil {
		t.Fatal(err)
	}
	if err := f.cons.PersistLedger(f.book); err != nil {
		t.Fatal(err)
	}
	recovery := newApplicationRecovery(f.book, f.attempts, f.tasks, f.c, nil, f.cons, i18n.New(i18n.LocaleEN), false)
	rejectCrashAccounting(t, f)
	if err := recovery.Reconcile(f.ctx); err == nil || !strings.Contains(err.Error(), "injected accounting crash") {
		t.Fatalf("fault missed acceptance-before-accounting window: %v", err)
	}
	queue := f.cons.Queue(crashConversation)
	if len(queue) != 1 || queue[0].State != consoleapi.ExchangeQueued || f.calls.Load() != 0 {
		t.Fatalf("not dormant: %+v calls=%d", queue, f.calls.Load())
	}
	// Public method called by POST /console/queue/{id}/steer and composer Send now.
	_, steerErr := f.cons.Steer(f.ctx, queue[0].ID)
	if steerErr == nil {
		f.waitTerminal(t)
	}
	dropCrashAccounting(t, f)
	if err := recovery.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.waitTerminal(t)
	row, _ := f.tasks.Get(original.TaskID)
	after := f.cons.Queue(crashConversation)
	if steerErr == nil || len(after) != 1 || after[0].State != consoleapi.ExchangeDone || row.Budget.Turns != 2 {
		t.Fatalf("Steer bypassed accounting barrier and consumed the only durable input: steerErr=%v calls=%d turns=%d oldOpen=%v queue=%+v", steerErr, f.calls.Load(), row.Budget.Turns, row.Attempts[0].Open(), after)
	}
}

type slowGatewayRecovery struct {
	turntest.IdleCoordinator
	entered chan struct{}
	release chan struct{}
}

func (p *slowGatewayRecovery) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	if req.OnTurnReady != nil {
		req.OnTurnReady(req.ExpectedTask, "gateway-attempt")
	}
	close(p.entered)
	select {
	case <-p.release:
		return turn.Result{Text: "gateway result", Attempt: "gateway-attempt"}, nil
	case <-ctx.Done():
		return turn.Result{}, ctx.Err()
	}
}

type schedulingChannel struct{ textOnlyGatewayChannel }

func (*schedulingChannel) Reply(context.Context, string, string) error { return nil }
func (*schedulingChannel) ReplyText(_ context.Context, _ string, text string) (string, error) {
	return "receipt-" + text, nil
}

func TestRunningRecoveryDoesNotWaitForGatewayNativeTurn(t *testing.T) {
	f := openCrashProbe(t, t.TempDir())
	defer f.close(t)
	if err := f.cons.PersistLedger(f.book); err != nil {
		t.Fatal(err)
	}
	p := &slowGatewayRecovery{entered: make(chan struct{}), release: make(chan struct{})}
	g := gateway.New(p)
	g.BindChannel(&schedulingChannel{})
	if err := g.QueueRecovery(f.ctx, f.book, "gateway-input", gateway.Revival{
		TaskID: "gateway-task", ConversationID: "independent", Member: "worker", MessageID: "anchor", Requester: "owner",
	}, ""); err != nil {
		t.Fatal(err)
	}
	r := newApplicationRecovery(f.book, f.attempts, f.tasks, f.c, g, f.cons, i18n.New(i18n.LocaleEN), false)
	defer func() { f.cancel(); r.workers.Close() }()
	pass := func() {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- r.Reconcile(f.ctx) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			f.cancel()
			<-done
			t.Fatal("accounting pass blocked on an independent native turn")
		}
	}
	pass()
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("gateway did not dispatch its accepted input")
	}
	original := f.seed(t, true, "")
	pass()
	tracked, _ := f.tasks.Get(original.TaskID)
	if tracked.Attempts[0].Open() || tracked.Budget.Tokens.Total != 18 || tracked.Budget.Turns != 1 {
		t.Fatalf("long gateway turn prevented independent exact accounting: %+v", tracked)
	}
	close(p.release)
	r.workers.group.Wait()
}
