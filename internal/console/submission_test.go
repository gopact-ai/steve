package console

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestCommandsAreScopedToConversationAndPersistTheirReply(t *testing.T) {
	h := &echo{}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	first, err := s.SendCommand(t.Context(), "one", "hello", "same-key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SendCommand(t.Context(), "two", "hello", "same-key")
	if err != nil || second.Conversation != "console:two" || first.ExchangeID == second.ExchangeID || len(h.seen) != 2 {
		t.Fatalf("command identity escaped conversation: first=%+v second=%+v err=%v calls=%d", first, second, err, len(h.seen))
	}
	restartedHandler := &echo{}
	restarted := New(restartedHandler, "owner", nil)
	if err := restarted.Persist(doc); err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.SendCommand(t.Context(), "one", "hello", "same-key")
	if err != nil || replayed.ID != first.ID || replayed.Text != first.Text || len(restartedHandler.seen) != 0 {
		t.Fatalf("restart replay executed again or lost reply: %+v, %v, calls=%d", replayed, err, len(restartedHandler.seen))
	}
	if _, err := restarted.SendCommand(t.Context(), "one", "different", "same-key"); !errors.Is(err, consoleapi.ErrCommandConflict) {
		t.Fatalf("same key changed payload: %v", err)
	}
}

func TestCommandKeySurvivesQueueEditsCancellationAndNormalization(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	first := enqueueForTest(t, s, "main", "block")
	active := nextCall(t, h)
	s.record(consoleapi.Reply{ID: "quoted", Conversation: "console:main", Text: "source"})
	initial, err := s.EnqueueCommand(t.Context(), "main", " line\r\nnext ", "key", []QuoteRef{{Conversation: "main", ReplyID: "quoted"}})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.EnqueueCommand(t.Context(), "console:main", "line\nnext", "key", []QuoteRef{{Conversation: "console:main", ReplyID: "quoted"}})
	if err != nil || replay.ID != initial.ID {
		t.Fatalf("normalized retry changed exchange: %+v, %v", replay, err)
	}
	if _, err := s.EnqueueCommand(t.Context(), "main", "line\nnext", "key", nil); !errors.Is(err, consoleapi.ErrCommandConflict) {
		t.Fatalf("changed quotes reused key: %v", err)
	}
	if _, err := s.EditQueued(initial.ID, "edited"); err != nil {
		t.Fatal(err)
	}
	replay, err = s.EnqueueCommand(t.Context(), "main", "line\nnext", "key", []QuoteRef{{Conversation: "main", ReplyID: "quoted"}})
	if err != nil || replay.ID != initial.ID || replay.Input != "edited" {
		t.Fatalf("editing destroyed submission identity: %+v, %v", replay, err)
	}
	if err := s.DeleteQueued(initial.ID); err != nil {
		t.Fatal(err)
	}
	replay, err = s.EnqueueCommand(t.Context(), "main", "line\nnext", "key", []QuoteRef{{Conversation: "main", ReplyID: "quoted"}})
	if err != nil || replay.ID != initial.ID || replay.State != "cancelled" {
		t.Fatalf("retry resurrected cancelled work: %+v, %v", replay, err)
	}
	active.finish <- nil
	awaitExchange(t, s, first.ID)
	noCall(t, h)
}

func TestRestartedCommandsReuseRunningAndQueuedExchanges(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	first, _ := s.EnqueueCommand(t.Context(), "main", "running", "running-key", nil)
	active := nextCall(t, h)
	waiting, _ := s.EnqueueCommand(t.Context(), "main", "waiting", "waiting-key", nil)
	raw, _, _ := doc.Load()
	active.finish <- nil
	second := nextCall(t, h)
	second.finish <- nil
	awaitExchange(t, s, waiting.ID)

	h2 := &queueHandler{started: make(chan *queueCall, 8)}
	restored := New(h2, "owner", nil)
	if err := restored.Persist(&memDoc{raw: raw, saved: true}); err != nil {
		t.Fatal(err)
	}
	firstReplay, err := restored.EnqueueCommand(t.Context(), "main", "running", "running-key", nil)
	if err != nil || firstReplay.ID != first.ID || firstReplay.State != "failed" {
		t.Fatalf("running command was replayed after restart: %+v, %v", firstReplay, err)
	}
	waitingReplay, err := restored.EnqueueCommand(t.Context(), "main", "waiting", "waiting-key", nil)
	if err != nil || waitingReplay.ID != waiting.ID || waitingReplay.State != "queued" {
		t.Fatalf("queued command duplicated: %+v, %v", waitingReplay, err)
	}
	if err := restored.Drain(); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h2)
	if call.req.Input != "waiting" {
		t.Fatalf("wrong command restarted: %q", call.req.Input)
	}
	answers := make(chan outcome, 2)
	for range 2 {
		go func() {
			r, err := restored.SendCommand(context.Background(), "main", "waiting", "waiting-key")
			answers <- outcome{r, err}
		}()
	}
	call.finish <- nil
	awaitExchange(t, restored, waiting.ID)
	for range 2 {
		got := <-answers
		if got.err != nil || got.reply.ExchangeID != waiting.ID {
			t.Fatalf("synchronous retry did not wait for accepted exchange: %+v", got)
		}
	}
	noCall(t, h2)
}

func TestFailedSubmissionDoesNotReserveCommandKey(t *testing.T) {
	h := &echo{}
	s := New(h, "owner", nil)
	doc := &brokenQueueDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	doc.muErr.Lock()
	doc.err = errors.New("save unavailable")
	doc.muErr.Unlock()
	if _, err := s.SendCommand(t.Context(), "main", "hello", "key"); err == nil {
		t.Fatal("failed save accepted a submission")
	}
	doc.muErr.Lock()
	doc.err = nil
	doc.muErr.Unlock()
	r, err := s.SendCommand(t.Context(), "main", "hello", "key")
	if err != nil || r.Text != "echo: hello" || len(h.seen) != 1 {
		t.Fatalf("failed acceptance poisoned key: %+v, %v calls=%d", r, err, len(h.seen))
	}
}

func TestKeyedReceiptsOutliveTranscriptProjection(t *testing.T) {
	h := &echo{}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	first, err := s.SendCommand(t.Context(), "main", "first", "keep-key")
	if err != nil {
		t.Fatal(err)
	}
	for i := range keep + 1 {
		if _, err := s.SendCommand(t.Context(), "main", fmt.Sprint(i), fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	h2 := &echo{}
	restarted := New(h2, "owner", nil)
	if err := restarted.Persist(doc); err != nil {
		t.Fatal(err)
	}
	r, err := restarted.SendCommand(t.Context(), "main", "first", "keep-key")
	if err != nil || r.ID != first.ID || len(h2.seen) != 0 {
		t.Fatalf("projection eviction removed command identity: %+v, %v", r, err)
	}
}

func TestTaskControlsAndAddressedCancelBypassBlockedWork(t *testing.T) {
	for _, input := range []string{"/tasks pause 12", "/tasks 12 cancel", "@codex /cancel", "@codex /tasks #12 暂停"} {
		t.Run(input, func(t *testing.T) {
			h := &queueHandler{started: make(chan *queueCall, 8)}
			s := New(h, "owner", nil)
			first := enqueueForTest(t, s, "main", "block")
			active := nextCall(t, h)
			s.record(consoleapi.Reply{ID: "quote", Conversation: "console:main", Text: "quoted source"})
			control, err := s.EnqueueCommand(t.Context(), "main", input, "control", []QuoteRef{{Conversation: "main", ReplyID: "quote"}})
			if err != nil {
				t.Fatal(err)
			}
			call := nextCall(t, h)
			if call.req.Input != input {
				t.Fatalf("quote hid the control command: %q", call.req.Input)
			}
			call.finish <- nil
			awaitExchange(t, s, control.ID)
			active.finish <- nil
			awaitExchange(t, s, first.ID)
		})
	}
}
