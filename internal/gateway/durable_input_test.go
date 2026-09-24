package gateway

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

type durableInputProbe struct {
	turntest.IdleCoordinator
	calls, resumes atomic.Int32
}

func (p *durableInputProbe) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	p.calls.Add(1)
	if req.OnTurnReady != nil {
		req.OnTurnReady("original-task", "original-attempt")
	}
	return turn.Result{Text: "complete original result", Attempt: "original-attempt"}, nil
}
func (p *durableInputProbe) ResumeRetainedChat(_ context.Context, id string, _ turn.Request) (turn.Result, error) {
	p.resumes.Add(1)
	return turn.Result{Text: "complete original result", Attempt: id}, nil
}

func inboundFixture() feishu.InboundMessage {
	return feishu.InboundMessage{ConversationID: "conversation", ChatID: "chat", MessageID: "input-message", SenderOpenID: "owner", Text: "original work", Mentioned: true}
}

func TestGatewayDurableInputRecoversAfterCompletionBeforeReply(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, ch := &durableInputProbe{}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_result_delivery BEFORE INSERT ON commands
		WHEN NEW.kind='gateway-input-reply' BEGIN SELECT RAISE(ABORT,'reply reservation unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.processAcceptedFixture(inboundFixture(), p); err == nil || !strings.Contains(err.Error(), "reply reservation unavailable") {
		t.Fatalf("completion-to-reply failure hidden: %v", err)
	}
	if p.calls.Load() != 1 || ch.results.Load() != 0 {
		t.Fatal("fixture did not stop after completion before delivery")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_result_delivery`); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	g = New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls.Load() != 1 || ch.results.Load() != 1 || ch.notices.Load() != 0 {
		t.Fatalf("recovery resubmitted work: calls=%d replies=%d notices=%d", p.calls.Load(), ch.results.Load(), ch.notices.Load())
	}
}

func TestGatewayDurableInputRecoversUnknownDispatchByOriginalReceipt(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p, ch := &durableInputProbe{}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_input_dispatch BEFORE UPDATE ON commands
		WHEN NEW.kind='gateway-input-dispatch' BEGIN SELECT RAISE(ABORT,'dispatch unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.processAcceptedFixture(inboundFixture(), p); err == nil {
		t.Fatal("dispatch receipt failure hidden")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_input_dispatch`); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 || p.resumes.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("wrong recovery: calls=%d resumes=%d replies=%d", p.calls.Load(), p.resumes.Load(), ch.results.Load())
	}
}

func TestGatewayDurableReplyUnknownStaysPendingAndVisible(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p, ch := &durableInputProbe{}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_input_reply BEFORE UPDATE ON commands
		WHEN NEW.kind='gateway-input-reply' BEGIN SELECT RAISE(ABORT,'reply receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.processAcceptedFixture(inboundFixture(), p); err == nil {
		t.Fatal("reply receipt failure hidden")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_input_reply`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); !errors.Is(err, channel.ErrOutcomeUnknown) {
			t.Fatalf("unknown delivery hidden: %v", err)
		}
	}
	if p.calls.Load() != 1 || ch.results.Load() != 1 {
		t.Fatal("unknown result delivery was repeated")
	}
}

func TestGatewayCompletedAttemptKeepsItsOriginalInputOwner(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p, ch := &durableInputProbe{}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_owned_reply BEFORE INSERT ON commands
		WHEN NEW.kind='gateway-input-reply' BEGIN SELECT RAISE(ABORT,'reply reservation unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.processAcceptedFixture(inboundFixture(), p); err == nil {
		t.Fatal("reply window not injected")
	}
	r := revivalFixture()
	r.TaskID, r.MessageID = "original-task", "input-message"
	if err := g.QueueRecovery(t.Context(), book, "task-result/original-attempt", r, "original-attempt"); err != nil {
		t.Fatal(err)
	}
	extra, err := book.PendingCommands(t.Context(), recoveryInputKind)
	if err != nil || len(extra) != 0 {
		t.Fatalf("one completion acquired a second executable delivery intent: %+v %v", extra, err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_owned_reply`); err != nil {
		t.Fatal(err)
	}
	if err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if ch.results.Load() != 1 {
		t.Fatalf("original result delivered %d times", ch.results.Load())
	}
}

// processAcceptedFixture accepts msg and consumes it on the calling
// goroutine. A dispatch that must be recovered from its admitted attempt
// resumes through driver, as ingress resumes it through the driver
// SetIngressLifetime wires.
func (g *Gateway) processAcceptedFixture(msg feishu.InboundMessage, driver RecoveryDriver) error {
	ctx, key, input := context.Background(), "gateway-input/"+msg.MessageID, gatewayInput{Message: msg}
	if err := g.acceptInput(ctx, key, input); err != nil {
		return err
	}
	claim, err := g.claimOrdinary(ctx, key, conversationID(input.Message), true, true)
	if err != nil {
		return err
	}
	defer claim.close()
	return g.consumeInput(ctx, g.recoveryLedger, key, input, driver, claim)
}
