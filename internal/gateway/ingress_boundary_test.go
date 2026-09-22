package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

func TestOrdinaryAcceptedInputSurvivesClosedLifetimeAndRestarts(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, ch := &durableInputProbe{}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	g.SetIngressLifetime(t.Context(), closedRecoveryWorkers{})
	if err := g.HandleMessage(inboundFixture()); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 0 || len(g.slots) != 0 || len(g.durableRunning) != 0 {
		t.Fatal("closed lifetime started or leaked an unjoined worker")
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
	if err := g.RecoverQueued(t.Context(), book, p, nil); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 || ch.results.Load() != 1 {
		t.Fatal("accepted input lost across shutdown")
	}
}

func TestOrdinaryAcknowledgementRefusalRestartsWithoutRepeatingReply(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, ch := &durableInputProbe{}, &recoveryChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_ack BEFORE UPDATE OF acknowledged_by ON commands
		WHEN NEW.kind='gateway-input' BEGIN SELECT RAISE(ABORT,'ack unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := g.processAcceptedFixture(inboundFixture()); err == nil {
		t.Fatal("ack refusal was hidden")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_ack`); err != nil {
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
	var workers recoveryTestWorkers
	for range 2 {
		if err := g.ReconcileQueued(t.Context(), book, p, nil, &workers); err != nil {
			t.Fatal(err)
		}
		workers.Wait()
	}
	pending, err := book.PendingCommands(t.Context(), gatewayInputKind)
	if err != nil || len(pending) != 0 || p.calls.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("ack retry repeated native/delivery: pending=%d native=%d results=%d err=%v", len(pending), p.calls.Load(), ch.results.Load(), err)
	}
}

func TestDurableActionRejectedAcceptanceDoesNotConsumeRetryOrRecall(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &ingressProbe{entered: make(chan turn.Request, 1)}
	ch := &actionRecallChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	var workers recoveryTestWorkers
	g.SetIngressLifetime(t.Context(), &workers)
	msg := inboundFixture()
	id := g.registerTurn(msg)
	g.setTurnCard(id, "card")
	g.finishTurn(id, true)
	action := feishu.CardAction{RequestID: id, MessageID: "card", ChatID: "chat", OpenID: "owner", Action: "turn_retry"}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_action BEFORE INSERT ON commands
		WHEN NEW.kind='gateway-input' BEGIN SELECT RAISE(ABORT,'accept unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if got := g.HandleCardAction(action); got.Type != "error" {
		t.Fatal(got)
	}
	if p.calls.Load() != 0 || ch.recalls.Load() != 0 {
		t.Fatal("refused action produced effects")
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_action`); err != nil {
		t.Fatal(err)
	}
	bad := action
	bad.MessageID = "another-card"
	if got := g.HandleCardAction(bad); got.Type != "error" {
		t.Fatal("unbound card authorized retry")
	}
	if got := g.HandleCardAction(action); got.Type != "success" {
		t.Fatalf("refusal consumed retry: %+v", got)
	}
	workers.Wait()
	if p.calls.Load() != 1 || ch.recalls.Load() != 1 {
		t.Fatal("stable card action not executed once")
	}
	inputs, _ := book.Commands(t.Context(), gatewayInputKind)
	if len(inputs) != 1 || inputs[0].ID != actionInputKey(action) {
		t.Fatal("card action acquired an unrelated intent")
	}
}

type actionRecallChannel struct {
	recoveryChannel
	recalls atomic.Int32
}

func (c *actionRecallChannel) DeleteMessage(context.Context, string) error {
	c.recalls.Add(1)
	return nil
}

type noReceiptChannel struct{ calls atomic.Int32 }

func (c *noReceiptChannel) Reply(context.Context, string, string) error { c.calls.Add(1); return nil }

func TestDurableResultRequiresRealProviderReceipt(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p, ch := &durableInputProbe{}, &noReceiptChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if err := g.processAcceptedFixture(inboundFixture()); err == nil {
		t.Fatal("fabricated successful provider receipt")
	}
	if ch.calls.Load() != 0 {
		t.Fatal("unsupported channel delivered without a receipt")
	}
	r, _, err := book.CommandReceipt(t.Context(), "gateway-input/input-message/reply")
	if err != nil || r.Error == "" {
		t.Fatalf("missing provider receipt became success: %+v %v", r, err)
	}
}

type ambiguousFinalChannel struct {
	recoveryChannel
	cards, patches atomic.Int32
}

func (c *ambiguousFinalChannel) ReplyCard(context.Context, string, []byte) (string, error) {
	c.cards.Add(1)
	return "opener", nil
}
func (c *ambiguousFinalChannel) PatchCard(context.Context, string, []byte) error {
	c.patches.Add(1)
	return io.EOF
}

func TestDurableUnknownFinalPatchNeverFallsBackToAnotherReply(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p, ch := &durableInputProbe{}, &ambiguousFinalChannel{}
	g := New(p)
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if err := g.processAcceptedFixture(inboundFixture()); !errors.Is(err, channel.ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	for range 2 {
		if err := g.RecoverQueued(t.Context(), book, p, nil); !errors.Is(err, channel.ErrOutcomeUnknown) {
			t.Fatal(err)
		}
	}
	if ch.cards.Load() != 1 || ch.patches.Load() != 1 || ch.results.Load()+ch.notices.Load() != 0 {
		t.Fatalf("unknown final patch blind-retried: cards=%d patches=%d replies=%d", ch.cards.Load(), ch.patches.Load(), ch.results.Load()+ch.notices.Load())
	}
}

type emptyListenProbe struct{}

func (emptyListenProbe) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, nil
}

func TestDurableSilentListeningRecordsPolicyNotExternalDelivery(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	ch := &recoveryChannel{}
	g := New(emptyListenProbe{})
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	msg := inboundFixture()
	msg.ChatType, msg.Mentioned = protocol.ChatGroup, false
	for range 2 {
		if err := g.processAcceptedFixture(msg); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := book.CommandReceipt(t.Context(), "gateway-input/input-message/reply"); err != nil || found {
		t.Fatal("silent policy forged a delivered reply")
	}
	pending, err := book.PendingCommands(t.Context(), gatewayInputKind)
	if err != nil || len(pending) != 0 || ch.results.Load()+ch.notices.Load() != 0 {
		t.Fatal("silent disposition was lost")
	}
}

func TestTopicOwnershipRejectsUnprovenOrUnrelatedRoute(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE commands SET finished_at=NULL WHERE kind='gateway-input-topic'`,
		`UPDATE commands SET error='unknown' WHERE kind='gateway-input-topic'`,
		`UPDATE commands SET actor='other' WHERE kind='gateway-input-topic'`,
		`UPDATE commands SET kind='other' WHERE kind='gateway-input-topic'`,
		`UPDATE commands SET result='{"input_id":"other","anchor":"topic-anchor","thread":"topic-thread"}' WHERE kind='gateway-input-topic'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			g := New(&durableInputProbe{})
			g.BindChannel(&ingressTopicChannel{})
			g.SetRecoveryLedger(book)
			msg := inboundFixture()
			msg.ConversationID, msg.Text = msg.ChatID, "/t original"
			if err := g.processAcceptedFixture(msg); err != nil {
				t.Fatal(err)
			}
			if _, err := book.DB().Exec(mutation); err != nil {
				t.Fatal(err)
			}
			ok, err := g.OwnsAttempt(t.Context(), book, "original-attempt", "original-task", "topic-thread", "topic-anchor", "owner")
			if err == nil || ok {
				t.Fatalf("unproven route authorized ownership: %t %v", ok, err)
			}
		})
	}
}

func TestOrdinaryAcceptanceConflictsOnActorPayloadOrKind(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	g := New(&durableInputProbe{})
	g.SetRecoveryLedger(book)
	g.SetIngressLifetime(t.Context(), closedRecoveryWorkers{})
	msg := inboundFixture()
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*feishu.InboundMessage){
		func(m *feishu.InboundMessage) { m.Text = "different" },
		func(m *feishu.InboundMessage) { m.SenderOpenID = "another" },
	} {
		other := msg
		mutate(&other)
		if err := g.HandleMessage(other); !errors.Is(err, ledger.ErrConflict) {
			t.Fatalf("key accepted another input: %v", err)
		}
	}
	raw, _ := json.Marshal(gatewayInput{Message: msg})
	if err := book.RecordCommand(t.Context(), "gateway-input/input-message", "another-kind", "owner", raw); !errors.Is(err, ledger.ErrConflict) {
		t.Fatal(err)
	}
}

func TestDurableHistoryCardReplaysItsOriginalAcceptanceOnly(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &ingressProbe{entered: make(chan turn.Request, 2)}
	g := New(p)
	g.BindChannel(&recoveryChannel{})
	g.SetRecoveryLedger(book)
	var workers recoveryTestWorkers
	g.SetIngressLifetime(t.Context(), &workers)
	action := feishu.CardAction{RequestID: "thread", Action: "history_restore", MessageID: "finished-card", ChatID: "chat", OpenID: "owner"}
	for range 2 {
		if toast := g.HandleCardAction(action); toast.Type != "success" {
			t.Fatal(toast)
		}
		workers.Wait()
	}
	if p.calls.Load() != 1 {
		t.Fatal("same rendered history action dispatched twice")
	}
	if req := nextIngress(t, p); req.Input != "/history 1" || req.ConversationID != "thread" || req.MessageID != "finished-card" {
		t.Fatal(req)
	}
	action.OpenID = "other"
	if toast := g.HandleCardAction(action); toast.Type != "error" {
		t.Fatal("accepted card identity was overwritten")
	}
}

type mismatchedIngressResult struct{ attempt string }

func (p mismatchedIngressResult) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	req.OnTurnReady("task", "admitted-attempt")
	return turn.Result{Text: "unsettled or unrelated result", Attempt: p.attempt}, nil
}

func TestDurableIngressCannotDeliverUnsettledOrDifferentAttemptResult(t *testing.T) {
	for _, returned := range []string{"", "different-attempt"} {
		t.Run(returned, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			ch := &recoveryChannel{}
			g := New(mismatchedIngressResult{attempt: returned})
			g.BindChannel(ch)
			g.SetRecoveryLedger(book)
			if err := g.processAcceptedFixture(inboundFixture()); err == nil {
				t.Fatal("unproven native result became delivery authority")
			}
			if _, exists, err := book.CommandReceipt(t.Context(), "gateway-input/input-message/reply"); err != nil || exists {
				t.Fatal("unsettled/mismatched attempt acquired a result receipt")
			}
			if ch.results.Load()+ch.notices.Load() != 0 {
				t.Fatal("unproven result delivered")
			}
		})
	}
}

func TestDurableNormalInputWaitsWithoutReservingDispatchBehindLiveOwner(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &ingressProbe{entered: make(chan turn.Request, 2), release: make(chan struct{})}
	g := New(p)
	g.BindChannel(&recoveryChannel{})
	g.SetRecoveryLedger(book)
	ctx, cancel := context.WithCancel(t.Context())
	var workers recoveryTestWorkers
	defer func() { cancel(); workers.Wait() }()
	g.SetIngressLifetime(ctx, &workers)
	first := inboundFixture()
	if err := g.HandleMessage(first); err != nil {
		t.Fatal(err)
	}
	nextIngress(t, p)
	second := first
	second.MessageID, second.Text = "later-message", "ordinary queued work"
	if err := g.HandleMessage(second); err != nil {
		t.Fatal(err)
	}
	if err := g.ReconcileQueued(ctx, book, nil, nil, &workers); err != nil {
		t.Fatal(err)
	}
	// A held owner must exclude reservation, not merely serialize native
	// execution after /dispatch has already made a queued input ambiguous.
	g.mu.Lock()
	observers := len(g.durableRunning)
	g.mu.Unlock()
	if observers != 1 {
		t.Fatalf("queued ordinary input got a live observer: %d", observers)
	}
	if _, exists, err := book.CommandReceipt(ctx, "gateway-input/later-message/dispatch"); err != nil || exists {
		t.Fatalf("queued ordinary input reserved dispatch prematurely: exists=%t err=%v", exists, err)
	}
	close(p.release)
	workers.Wait()
	if err := g.ReconcileQueued(ctx, book, nil, nil, &workers); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if req := nextIngress(t, p); req.MessageID != second.MessageID {
		t.Fatal(req)
	}
}

type rejectedIngressProbe struct{}

func (rejectedIngressProbe) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, &turn.RecoveryBlocked{Question: view.Question{Message: "previous execution requires reconciliation"}}
}

func TestDurableObservedPreAdmissionRejectionIsNotUnknownNativeDispatch(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	ch := &recoveryChannel{}
	g := New(rejectedIngressProbe{})
	g.BindChannel(ch)
	g.SetRecoveryLedger(book)
	if err := g.processAcceptedFixture(inboundFixture()); err != nil {
		t.Fatalf("observed rejection without any OnTurnReady stranded as unknown native execution: %v", err)
	}
	if _, exists, _ := book.CommandReceipt(t.Context(), "gateway-input/input-message/attempt"); exists {
		t.Fatal("rejection invented a native attempt")
	}
	pending, err := book.PendingCommands(t.Context(), gatewayInputKind)
	if err != nil || len(pending) != 0 || ch.notices.Load() != 1 {
		t.Fatal("pre-admission rejection was not reported once")
	}
}

func TestDurableTopicSeedsParallelThreadWhileOriginalChatIsServing(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	p := &ingressProbe{entered: make(chan turn.Request, 2), release: make(chan struct{})}
	g := New(p)
	g.slots = make(chan struct{}, 2)
	g.BindChannel(&ingressTopicChannel{})
	g.SetRecoveryLedger(book)
	ctx, cancel := context.WithCancel(t.Context())
	var workers recoveryTestWorkers
	defer func() { cancel(); workers.Wait() }()
	g.SetIngressLifetime(ctx, &workers)
	msg := inboundFixture()
	msg.ConversationID = msg.ChatID
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	nextIngress(t, p)
	msg.MessageID, msg.Text = "topic-input", "/t parallel work"
	if err := g.HandleMessage(msg); err != nil {
		t.Fatal(err)
	}
	req := nextIngress(t, p)
	if req.ConversationID != "topic-thread" || req.Input != "parallel work" {
		t.Fatal(req)
	}
	select {
	case <-p.release:
		t.Fatal("topic waited for the original chat's native completion")
	default:
	}
	close(p.release)
	workers.Wait()
	if len(g.slots) != 0 {
		t.Fatal("topic route transfer leaked a slot")
	}
}

type ingressTextReceipt struct{ texts []string }

func (c *ingressTextReceipt) Reply(context.Context, string, string) error {
	return errors.New("non-receipted reply")
}
func (c *ingressTextReceipt) ReplyText(_ context.Context, _ string, text string) (string, error) {
	c.texts = append(c.texts, text)
	return "reply-receipt", nil
}

func TestDurableResultRetainsUserErrorAndCancellationRendering(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		want    string
	}{
		{"user-error", turn.UserError{Text: "expected actionable rejection"}, "expected actionable rejection"},
		{"cancelled", context.Canceled, i18n.New(i18n.LocaleZH).T(i18n.TurnCanceled)},
	} {
		t.Run(test.name, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			ch := &ingressTextReceipt{}
			g := New(failedContinuationProcessor{failure: test.failure})
			g.BindChannel(ch)
			g.SetRecoveryLedger(book)
			if _, err := book.DB().Exec(`CREATE TRIGGER defer_rendering BEFORE INSERT ON commands
				WHEN NEW.kind='gateway-input-reply' BEGIN SELECT RAISE(ABORT,'defer reply'); END`); err != nil {
				t.Fatal(err)
			}
			if err := g.processAcceptedFixture(inboundFixture()); err == nil {
				t.Fatal("failed to defer rendering")
			}
			if _, err := book.DB().Exec(`DROP TRIGGER defer_rendering`); err != nil {
				t.Fatal(err)
			}
			if err := g.RecoverQueued(t.Context(), book, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(ch.texts) != 1 || ch.texts[0] != test.want {
				t.Fatalf("durable result changed user-facing error: got=%v want=%q", ch.texts, test.want)
			}
		})
	}
}
