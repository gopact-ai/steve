package gateway

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// Without Feishu the gateway has no channel, yet it still owns durable input
// and task recovery. These tests pin what that state does.

func openNilChannelBook(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	return book
}

func TestNilChannelDurableInputRunsButCannotDeliver(t *testing.T) {
	book := openNilChannelBook(t)
	p := &durableInputProbe{}
	g := New(p)
	g.SetRecoveryLedger(book)
	if err := g.processAcceptedFixture(inboundFixture()); err == nil {
		t.Fatal("delivery without a channel reported success")
	}
	if p.calls.Load() != 1 {
		t.Fatalf("accepted input dispatched %d times, want 1", p.calls.Load())
	}
	r, _, err := book.CommandReceipt(t.Context(), "gateway-input/input-message/reply")
	if err != nil || !strings.Contains(r.Error, "gateway reply channel is not available") {
		t.Fatalf("reply receipt = %+v, %v", r, err)
	}
}

func TestNilChannelDurableTopicIsRefusedWithoutDispatch(t *testing.T) {
	book := openNilChannelBook(t)
	p := &durableInputProbe{}
	g := New(p)
	g.SetRecoveryLedger(book)
	msg := inboundFixture()
	msg.ConversationID, msg.Text = msg.ChatID, "/t split the work"
	if err := g.processAcceptedFixture(msg); err == nil {
		t.Fatal("delivery without a channel reported success")
	}
	if p.calls.Load() != 0 {
		t.Fatal("a topic that cannot be seeded reached the processor")
	}
	dispatch, found, err := book.CommandReceipt(t.Context(), "gateway-input/input-message/dispatch")
	if err != nil || !found || !strings.Contains(string(dispatch.Result), g.text.T(i18n.TopicFailed)) {
		t.Fatalf("dispatch receipt = %+v, %v, %v; want the topic refusal", dispatch, found, err)
	}
	if _, seeded, err := book.CommandReceipt(t.Context(), "gateway-input/input-message/topic"); err != nil || seeded {
		t.Fatalf("topic seed reserved without a channel: %v, %v", seeded, err)
	}
}

func TestNilChannelRecoveryIsRefusedBeforeRevivalOrDispatch(t *testing.T) {
	book := openNilChannelBook(t)
	p := &recoveryProbe{}
	g := New(p)
	if err := g.QueueRecovery(t.Context(), book, "restart:parent", revivalFixture(), ""); err != nil {
		t.Fatal(err)
	}
	revived := false
	err := g.RecoverQueued(t.Context(), book, p, func(string, string) error { revived = true; return nil })
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
	if id := g.ack("om_1"); id != "" {
		t.Fatalf("ack without a channel = %q", id)
	}
	g.unack("om_1", "rx_1")
	g.recall("om_card")
	ui := g.newTurnUI(inboundFixture(), false)
	if ui.cardID != "" || ui.fallback || ui.reaction != "" {
		t.Fatalf("turn UI without a channel = card %q fallback %v reaction %q", ui.cardID, ui.fallback, ui.reaction)
	}
	err := g.processTask(inboundFixture(), "")
	if err == nil || !strings.Contains(err.Error(), "gateway reply channel is not available") || p.calls.Load() != 1 {
		t.Fatalf("turn without a channel = %v after %d calls", err, p.calls.Load())
	}
	if _, err := g.newResultUI(inboundFixture()).finish(turn.Result{Text: "retained"}, nil); err == nil ||
		!strings.Contains(err.Error(), "gateway reply channel is not available") {
		t.Fatalf("retained result without a channel = %v", err)
	}
	topic := inboundFixture()
	topic.ConversationID, topic.Text = topic.ChatID, "/t split the work"
	if err := g.processTask(topic, ""); err != nil || p.calls.Load() != 1 {
		t.Fatalf("topic without a channel = %v after %d calls", err, p.calls.Load())
	}
}
