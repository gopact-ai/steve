package app

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
)

// Native completion already exists. The only substituted dependency is native
// admission; attempt evidence, task accounting, gateway input and recovery all
// use their production owners and the same real SQLite ledger.
type ordinaryCompletionProbe struct {
	coordinator   *turn.Coordinator
	task, attempt string
	calls         atomic.Int32
}

func (p *ordinaryCompletionProbe) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	p.calls.Add(1)
	req.OnTurnReady(p.task, p.attempt)
	return p.coordinator.ResumeRetainedChat(ctx, p.attempt, req)
}

func TestOrdinaryFeishuIngressRecoversOriginalCompletionAndAccountingAfterReopen(t *testing.T) {
	dir := t.TempDir()
	first := openCrashProbe(t, dir)
	r := first.seed(t, true, "", "feishu")
	rejectCrashAccounting(t, first)
	p := &ordinaryCompletionProbe{coordinator: first.c, task: r.TaskID, attempt: r.ID}
	ch := &crashGatewayChannel{}
	g := gateway.New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(first.book)
	workers := &reconciliationWorkers{}
	g.SetIngressLifetime(first.ctx, workers)
	err := g.HandleMessage(feishu.InboundMessage{
		ConversationID: crashConversation, ChatID: "console", MessageID: "web-original",
		SenderOpenID: "owner", Text: "original goal", Mentioned: true, ChatType: protocol.ChatP2P,
	})
	if err != nil {
		t.Fatal(err)
	}
	workers.Close()
	if p.calls.Load() != 1 || ch.replies.Load() != 0 {
		t.Fatal("did not stop between completion and accounting")
	}
	receipt, exists, err := first.book.CommandReceipt(first.ctx, "gateway-input/web-original/attempt")
	if err != nil || !exists || receipt.FinishedAt == nil || receipt.Error != "" {
		t.Fatalf("real ingress did not persist original execution owner: %+v %v", receipt, err)
	}
	first.close(t)

	recovered := openCrashProbe(t, dir)
	defer recovered.close(t)
	dropCrashAccounting(t, recovered)
	if err := recovered.cons.PersistLedger(recovered.book); err != nil {
		t.Fatal(err)
	}
	g = gateway.New(recovered)
	g.BindChannel(ch)
	g.SetRecoveryLedger(recovered.book)
	recovery := newApplicationRecovery(recovered.book, recovered.attempts, recovered.tasks, recovered.c, g, recovered.cons, i18n.New(i18n.LocaleEN), false)
	defer recovery.workers.Close()
	for range 2 {
		if err := recovery.Reconcile(recovered.ctx); err != nil {
			t.Fatal(err)
		}
		recovery.workers.group.Wait()
	}
	row, _ := recovered.tasks.Get(r.TaskID)
	pending, err := recovered.book.PendingCommands(recovered.ctx, "gateway-input")
	if err != nil || len(pending) != 0 || ch.replies.Load() != 1 || recovered.calls.Load() != 0 ||
		row.Budget.Turns != 1 || row.Budget.Tokens.Total != 18 || row.Attempts[0].Open() {
		t.Fatalf("ordinary recovery repeated native/charge or lost result: pending=%d replies=%d new_handle=%d task=%+v err=%v", len(pending), ch.replies.Load(), recovered.calls.Load(), row, err)
	}
	if _, exists, err := recovered.book.CommandReceipt(recovered.ctx, "task-result/"+r.ID); err != nil || exists {
		t.Fatalf("completion acquired another executable intent: exists=%t err=%v", exists, err)
	}
}
