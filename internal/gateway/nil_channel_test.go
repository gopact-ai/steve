package gateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// Without Feishu the gateway has no channel, yet it still owns durable input
// and task recovery, which wait for a gateway that has one. These tests pin
// what that state does.

func openNilChannelBook(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	return book
}

// An accepted input, flat or /t, is neither run nor answered without a
// channel. It stays pending with no step recorded, so a gateway that has a
// channel later seeds, runs and answers it once. An input dispatched before
// with no recorded outcome is not observed without a channel either; the
// gateway that has one observes the original attempt and answers it without
// running the input again.
func TestNilChannelDurableInputWaitsForAChannel(t *testing.T) {
	for name, tc := range map[string]struct {
		text  string
		seeds int32
	}{
		"ordinary": {"original work", 0},
		"topic":    {"/t split the work", 1},
	} {
		t.Run(name, func(t *testing.T) {
			book := openNilChannelBook(t)
			p := &durableInputProbe{}
			g := New(p)
			g.SetRecoveryLedger(book)
			msg := inboundFixture()
			msg.ConversationID, msg.Text = msg.ChatID, tc.text
			if err := g.processAcceptedFixture(msg, p); err == nil ||
				!strings.Contains(err.Error(), "gateway reply channel is not available") {
				t.Fatalf("input without a channel = %v; want the missing channel", err)
			}
			if p.calls.Load() != 0 || pendingInputs(t, book) != 1 {
				t.Fatalf("without a channel: calls=%d pending=%d; want 0 and 1", p.calls.Load(), pendingInputs(t, book))
			}
			for _, step := range []string{"dispatch", "topic", "reply"} {
				if _, found, err := book.CommandReceipt(t.Context(), "gateway-input/input-message/"+step); err != nil || found {
					t.Fatalf("%s recorded without a channel: %v, %v", step, found, err)
				}
			}
			ch := &ingressTopicChannel{}
			later := New(p)
			later.BindChannel(ch)
			later.SetRecoveryLedger(book)
			if err := later.recoverQueuedFixture(t.Context(), book, p, nil); err != nil {
				t.Fatalf("recovery with a channel: %v", err)
			}
			if pendingInputs(t, book) != 0 || p.calls.Load() != 1 || ch.seeds.Load() != tc.seeds || ch.results.Load() != 1 {
				t.Fatalf("with a channel: pending=%d calls=%d seeds=%d results=%d; want 0, 1, %d, 1",
					pendingInputs(t, book), p.calls.Load(), ch.seeds.Load(), ch.results.Load(), tc.seeds)
			}
		})
	}
	t.Run("dispatched without a reply", func(t *testing.T) {
		book := openNilChannelBook(t)
		p, ch := &durableInputProbe{}, &recoveryChannel{}
		g := New(p)
		g.BindChannel(ch)
		g.SetRecoveryLedger(book)
		// The dispatch receipt cannot be finished, as when the process stops
		// after the attempt was admitted.
		if _, err := book.DB().Exec(`CREATE TRIGGER reject_input_dispatch BEFORE UPDATE ON commands
			WHEN NEW.kind='gateway-input-dispatch' BEGIN SELECT RAISE(ABORT,'dispatch unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		if err := g.processAcceptedFixture(inboundFixture(), p); err == nil || !strings.Contains(err.Error(), "dispatch unavailable") {
			t.Fatalf("unfinished dispatch = %v", err)
		}
		if _, err := book.DB().Exec(`DROP TRIGGER reject_input_dispatch`); err != nil {
			t.Fatal(err)
		}
		without := New(p)
		without.SetRecoveryLedger(book)
		if err := without.recoverQueuedFixture(t.Context(), book, p, nil); err == nil ||
			!strings.Contains(err.Error(), "gateway reply channel is not available") {
			t.Fatalf("dispatched input without a channel = %v; want the missing channel", err)
		}
		if p.calls.Load() != 1 || p.resumes.Load() != 0 || pendingInputs(t, book) != 1 {
			t.Fatalf("without a channel: calls=%d resumes=%d pending=%d; want 1, 0 and 1",
				p.calls.Load(), p.resumes.Load(), pendingInputs(t, book))
		}
		if _, found, err := book.CommandReceipt(t.Context(), "gateway-input/input-message/reply"); err != nil || found {
			t.Fatalf("reply recorded without a channel: %v, %v", found, err)
		}
		later := New(p)
		later.BindChannel(ch)
		later.SetRecoveryLedger(book)
		if err := later.recoverQueuedFixture(t.Context(), book, p, nil); err != nil {
			t.Fatalf("recovery with a channel: %v", err)
		}
		if pendingInputs(t, book) != 0 || p.calls.Load() != 1 || p.resumes.Load() != 1 || ch.results.Load() != 1 {
			t.Fatalf("with a channel: pending=%d calls=%d resumes=%d results=%d; want 0, 1, 1, 1",
				pendingInputs(t, book), p.calls.Load(), p.resumes.Load(), ch.results.Load())
		}
	})
}

func TestNilChannelRecoveryIsRefusedBeforeRevivalOrDispatch(t *testing.T) {
	book := openNilChannelBook(t)
	p := &recoveryProbe{}
	g := New(p)
	if err := g.QueueRecovery(t.Context(), book, "restart:parent", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	revived := false
	err := g.recoverQueuedFixture(t.Context(), book, p, func(string, string) error { revived = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "gateway recovery reply channel is not available") {
		t.Fatalf("recovery without a channel = %v", err)
	}
	if revived || p.calls.Load() != 0 || p.resumes.Load() != 0 {
		t.Fatal("recovery without a channel revived or dispatched work")
	}
	if _, noticed, err := book.CommandReceipt(t.Context(), "restart:parent/notice"); err != nil || noticed {
		t.Fatalf("notice reserved without a channel: %v, %v", noticed, err)
	}
}

func TestNilChannelRefusesNoticesWithoutProcessing(t *testing.T) {
	book := openNilChannelBook(t)
	p := &countingProcessor{}
	g := New(p)
	anchor := revivalFixture()
	g.Notify(Notice{TaskID: anchor.TaskID, MessageID: anchor.MessageID, Requester: anchor.Requester, Text: "done"})
	if err := g.Deliver(anchor, "notice", "prompt"); err == nil {
		t.Fatal("delivery without a channel was accepted")
	}
	confirmed := false
	if err := g.DeliverConfirmed(anchor, "notice", "prompt", func(error) { confirmed = true }); err == nil || confirmed {
		t.Fatalf("confirmed delivery without a channel = %v, confirmed=%v", err, confirmed)
	}
	if _, err := g.FireSchedule(t.Context(), testFire()); err == nil {
		t.Fatal("schedule fired without a channel")
	}
	admission := task.ResumeAdmission{ID: "control", TaskID: anchor.TaskID, Epoch: 1}
	if err := g.QueueTaskResume(t.Context(), book, admission.ID, anchor, admission); err == nil {
		t.Fatal("task resume accepted without a channel")
	}
	if _, found, err := book.CommandReceipt(t.Context(), admission.ID); err != nil || found {
		t.Fatalf("task resume recorded without a channel: %v, %v", found, err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("a refused notice reached the processor")
	}
}

func TestNilChannelTurnRunsWithoutCardOrReactionAndFailsDelivery(t *testing.T) {
	p := &countingProcessor{}
	g := New(p)
	if id := ack(g.channel(), "om_1"); id != "" {
		t.Fatalf("ack without a channel = %q", id)
	}
	unack(g.channel(), "om_1", "rx_1")
	recall(g.channel(), "om_card")
	ui := g.newTurnUI(g.channel(), inboundFixture(), false)
	if ui.cardID != "" || ui.fallback || ui.reaction != "" {
		t.Fatalf("turn UI without a channel = card %q fallback %v reaction %q", ui.cardID, ui.fallback, ui.reaction)
	}
	err := g.processTask(inboundFixture(), "")
	if err == nil || !strings.Contains(err.Error(), "gateway reply channel is not available") || p.calls.Load() != 1 {
		t.Fatalf("turn without a channel = %v after %d calls", err, p.calls.Load())
	}
	if _, err := g.newResultUI(g.channel(), inboundFixture()).finish(turn.Result{Text: "retained"}, nil); err == nil ||
		!strings.Contains(err.Error(), "gateway reply channel is not available") {
		t.Fatalf("retained result without a channel = %v", err)
	}
	topic := inboundFixture()
	topic.ConversationID, topic.Text = topic.ChatID, "/t split the work"
	if err := g.processTask(topic, ""); err != nil || p.calls.Load() != 1 {
		t.Fatalf("topic without a channel = %v after %d calls", err, p.calls.Load())
	}
}

// Feishu is bound once its identity is verified, which may be long after
// recovery, schedules and notices began using the same gateway, and while
// they run. Until then accepted input waits with no step recorded; the first
// pass after the bind runs and answers it once.
func TestLateBoundChannelTakesOverWaitingInput(t *testing.T) {
	book := openNilChannelBook(t)
	p := &durableInputProbe{}
	g := New(p)
	g.SetRecoveryLedger(book)
	if err := g.processAcceptedFixture(inboundFixture(), p); err == nil ||
		!strings.Contains(err.Error(), "gateway reply channel is not available") {
		t.Fatalf("input before the bind = %v; want the missing channel", err)
	}
	ch := &ingressTopicChannel{}
	// Incomplete, so it is refused after the channel check, never run.
	incomplete := testFire()
	incomplete.Prompt = ""
	passes := make(chan struct{})
	go func() {
		defer close(passes)
		for range 20 {
			_ = g.recoverQueuedFixture(t.Context(), book, p, nil)
			g.Notify(Notice{TaskID: "task", MessageID: "anchor", Text: "done"})
			_, _ = g.FireSchedule(t.Context(), incomplete)
		}
	}()
	g.BindChannel(ch)
	<-passes
	if err := g.recoverQueuedFixture(t.Context(), book, p, nil); err != nil {
		t.Fatalf("recovery after the bind: %v", err)
	}
	if pendingInputs(t, book) != 0 || p.calls.Load() != 1 || ch.results.Load() != 1 {
		t.Fatalf("after the bind: pending=%d calls=%d results=%d; want 0, 1, 1",
			pendingInputs(t, book), p.calls.Load(), ch.results.Load())
	}
}

// bindingProcessor binds a channel to its gateway while it handles a turn.
type bindingProcessor struct {
	turntest.IdleCoordinator
	g  *Gateway
	ch Channel
}

func (p *bindingProcessor) Handle(context.Context, turn.Request) (turn.Result, error) {
	p.g.BindChannel(p.ch)
	return turn.Result{Text: "answer"}, nil
}

// postCounter counts every message posted through it.
type postCounter struct {
	nopChannel
	posts atomic.Int32
}

func (c *postCounter) Reply(context.Context, string, string) error { c.posts.Add(1); return nil }
func (c *postCounter) ReplyText(context.Context, string, string) (string, error) {
	c.posts.Add(1)
	return "posted", nil
}

// A turn that began without a channel ends without one: a channel bound
// while it runs does not post an answer for a turn that showed no card or
// reaction, which leaves the input to the pass that has a channel.
func TestTurnKeepsTheChannelItBeganWith(t *testing.T) {
	ch := &postCounter{}
	p := &bindingProcessor{ch: ch}
	g := New(p)
	p.g = g
	err := g.processTask(inboundFixture(), "")
	if err == nil || !strings.Contains(err.Error(), "gateway reply channel is not available") || ch.posts.Load() != 0 {
		t.Fatalf("turn bound midway = %v with %d posts; want the missing channel and none", err, ch.posts.Load())
	}
}
