package gateway

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

func TestHandleMessageRejectsFailedDurableAcceptance(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &durableInputProbe{}
	g := New(p)
	g.SetRecoveryLedger(book)
	g.SetIngressLifetime(t.Context(), closedRecoveryWorkers{}, p)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_ingress BEFORE INSERT ON commands
		WHEN NEW.kind='gateway-input' BEGIN SELECT RAISE(ABORT,'input unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.HandleMessage(inboundFixture()); err == nil || !strings.Contains(err.Error(), "input unavailable") {
		t.Fatalf("real ingress did not return the durable acceptance refusal: %v", err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("failed acceptance entered native execution")
	}
}

type ingressProbe struct {
	turntest.IdleCoordinator
	calls   atomic.Int32
	entered chan turn.Request
	release chan struct{}
}

func (p *ingressProbe) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	p.calls.Add(1)
	if req.OnTurnReady != nil && req.Input != "/cancel" {
		req.OnTurnReady("task-"+req.MessageID, "attempt-"+req.MessageID)
	}
	p.entered <- req
	if req.Input != "/cancel" && p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return turn.Result{}, ctx.Err()
		}
	}
	attempt := "attempt-" + req.MessageID
	if req.Input == "/cancel" {
		attempt = ""
	}
	return turn.Result{Text: "complete original result", Attempt: attempt}, nil
}

func nextIngress(t *testing.T, p *ingressProbe) turn.Request {
	t.Helper()
	select {
	case req := <-p.entered:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("accepted input did not enter the processor")
		return turn.Request{}
	}
}

func TestOrdinaryIngressReturnsAfterAcceptanceAndSharesRecoveryControlSlot(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &ingressProbe{entered: make(chan turn.Request, 4), release: make(chan struct{})}
	g := New(p)
	g.slots = make(chan struct{}, 1)
	g.BindChannel(&recoveryChannel{})
	g.SetRecoveryLedger(book)
	ctx, cancel := context.WithCancel(t.Context())
	var workers recoveryTestWorkers
	defer func() { cancel(); workers.Wait() }()
	g.SetIngressLifetime(ctx, &workers, nil)
	msg := inboundFixture()
	done := make(chan error, 1)
	go func() { done <- g.HandleMessage(msg) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback waited across native Handle")
	}
	nextIngress(t, p)
	// Duplicate live ingress and the runtime observer must not join the
	// same input twice, while a distinct stop uses its conversation owner.
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := g.ReconcileQueued(ctx, book, nil, nil, &workers); err != nil {
		t.Fatal(err)
	}
	stop := msg
	stop.MessageID, stop.Text = "stop", "/cancel"
	if err := g.HandleMessage(stop); err != nil {
		t.Fatal(err)
	}
	if req := nextIngress(t, p); req.Input != "/cancel" {
		t.Fatalf("duplicate dispatch: %+v", req)
	}
	other := msg
	other.MessageID, other.ConversationID = "other", "other"
	if err := g.HandleMessage(other); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 2 {
		t.Fatal("saturated capacity dispatched another conversation")
	}
	close(p.release)
	workers.Wait()
	if err := g.ReconcileQueued(ctx, book, nil, nil, &workers); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if req := nextIngress(t, p); req.MessageID != "other" {
		t.Fatal(req)
	}
	if len(g.slots) != 0 || len(g.durableRunning) != 0 {
		t.Fatal("observer leaked owner")
	}
}

type ingressTopicChannel struct {
	recoveryChannel
	seeds atomic.Int32
}

func (c *ingressTopicChannel) ReplyThread(context.Context, string, string) (string, string, error) {
	c.seeds.Add(1)
	return "topic-anchor", "topic-thread", nil
}

func TestOrdinaryTopicUsesOneAcceptedInputAndMovesItsControlSlot(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &ingressProbe{entered: make(chan turn.Request, 3), release: make(chan struct{})}
	ch := &ingressTopicChannel{}
	g := New(p)
	g.slots = make(chan struct{}, 1)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	var workers recoveryTestWorkers
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); workers.Wait() }()
	g.SetIngressLifetime(ctx, &workers, nil)
	msg := inboundFixture()
	msg.ConversationID, msg.Text = msg.ChatID, "/t original work"
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	req := nextIngress(t, p)
	if req.ConversationID != "topic-thread" || req.MessageID != "topic-anchor" || req.Input != "original work" {
		t.Fatalf("topic command reached native rather than its proven route: %+v", req)
	}
	inputs, err := book.Commands(ctx, gatewayInputKind)
	if err != nil || len(inputs) != 1 {
		t.Fatalf("topic created multiple accepted intents: %d %v", len(inputs), err)
	}
	if _, exists, err := book.CommandReceipt(ctx, "gateway-input/input-message/attempt"); err != nil || !exists {
		t.Fatalf("attempt lost original input owner: %t %v", exists, err)
	}
	stop := msg
	stop.MessageID, stop.ConversationID, stop.Text = "stop", "topic-thread", "/cancel"
	if err := g.HandleMessage(stop); err != nil {
		t.Fatal(err)
	}
	if req := nextIngress(t, p); req.Input != "/cancel" {
		t.Fatal(req)
	}
	close(p.release)
	workers.Wait()
	owned, err := g.OwnsAttempt(ctx, book, "attempt-topic-anchor", "task-topic-anchor", "topic-thread", "topic-anchor", "owner")
	if err != nil || !owned {
		t.Fatalf("topic proof lost result ownership: %t %v", owned, err)
	}
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if ch.seeds.Load() != 1 || p.calls.Load() != 2 {
		t.Fatalf("topic redelivery reexecuted: seeds=%d calls=%d", ch.seeds.Load(), p.calls.Load())
	}
}

func TestOrdinaryTopicUnknownSeedDoesNotDispatchOrReseed(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p, ch := &durableInputProbe{}, &ingressTopicChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_topic_receipt BEFORE UPDATE ON commands
		WHEN NEW.kind='gateway-input-topic' BEGIN SELECT RAISE(ABORT,'topic receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	msg := inboundFixture()
	msg.ConversationID, msg.Text = msg.ChatID, "/t original work"
	if err := g.processAcceptedFixture(msg, p); err == nil {
		t.Fatal("seed receipt fault was hidden")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_topic_receipt`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, nil); !errors.Is(err, channel.ErrOutcomeUnknown) {
			t.Fatalf("unknown topic receipt was cleared: %v", err)
		}
	}
	if ch.seeds.Load() != 1 || p.calls.Load() != 0 {
		t.Fatalf("seed replayed or native dispatched: %d %d", ch.seeds.Load(), p.calls.Load())
	}
}

func TestDurableCardRetryDoesNotReuseOriginalMessageCommand(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &ingressProbe{entered: make(chan turn.Request, 4)}
	g := New(p)
	g.BindChannel(&recoveryChannel{})
	g.SetRecoveryLedger(book)
	var workers recoveryTestWorkers
	g.SetIngressLifetime(t.Context(), &workers, nil)
	msg := inboundFixture()
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	nextIngress(t, p)
	id := g.registerTurn(msg)
	g.setTurnCard(id, "failed-card")
	g.finishTurn(id, true)
	action := feishu.CardAction{OpenID: "owner", ChatID: msg.ChatID, MessageID: "failed-card", RequestID: id, Action: "turn_retry"}
	wrong := action
	wrong.OpenID = "intruder"
	if toast := g.HandleCardAction(wrong); toast.Type != "error" {
		t.Fatalf("wrong actor authorized: %+v", toast)
	}
	if toast := g.HandleCardAction(action); toast.Type != "success" {
		t.Fatalf("unauthorized tap consumed retry: %+v", toast)
	}
	workers.Wait()
	if p.calls.Load() != 2 {
		t.Fatalf("retry replayed original command: calls=%d", p.calls.Load())
	}
	if req := nextIngress(t, p); req.Input != msg.Text || req.MessageID != msg.MessageID {
		t.Fatalf("retry changed native anchor: %+v", req)
	}
	g.HandleCardAction(action)
	workers.Wait()
	if p.calls.Load() != 2 {
		t.Fatal("same card retry dispatched twice")
	}
}

func TestRuntimeReconcilerConsumesAcceptedOrdinaryInput(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p, ch := &durableInputProbe{}, &recoveryChannel{}
	g := New(p)
	g.SetRecoveryLedger(book)
	g.BindChannel(ch)
	if err := g.acceptInput(t.Context(), "gateway-input/input-message", gatewayInput{Message: inboundFixture()}); err != nil {
		t.Fatal(err)
	}
	var workers recoveryTestWorkers
	if err := g.ReconcileQueued(t.Context(), book, p, func(string, string) error { return nil }, &workers); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if p.calls.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("production reconciler ignores ordinary pending input: calls=%d results=%d", p.calls.Load(), ch.results.Load())
	}
}

// An accepted input whose turn does not settle the attempt it admitted is
// recovered through the driver its ingress is given, though the coordinator
// itself resumes no retained chat.
func TestIngressRecoversAnUnsettledTurnThroughItsDriver(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	driver, ch := &recoveryProbe{}, &recoveryChannel{}
	g := New(mismatchedIngressResult{})
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	var workers recoveryTestWorkers
	g.SetIngressLifetime(t.Context(), &workers, driver)
	if err := g.HandleMessage(inboundFixture()); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if driver.resumes.Load() != 1 || driver.calls.Load() != 0 || ch.results.Load() != 1 {
		t.Fatalf("driver resumed %d chats and ran %d turns, %d results delivered; want 1, 0 and 1",
			driver.resumes.Load(), driver.calls.Load(), ch.results.Load())
	}
	if pending, err := book.PendingCommands(t.Context(), gatewayInputKind); err != nil || len(pending) != 0 {
		t.Fatalf("pending inputs = %d, %v; want none", len(pending), err)
	}
}
