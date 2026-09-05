package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type queueCall struct {
	req      turn.Request
	finish   chan error
	canceled chan struct{}
}

// queueHandler lets a test separately control a turn's cancellation and
// cleanup. A replacement must not drain the FIFO while cleanup still runs.
type queueHandler struct{ started chan *queueCall }

func (h *queueHandler) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	call := &queueCall{req: req, finish: make(chan error, 1), canceled: make(chan struct{})}
	h.started <- call
	if req.OnProgress != nil {
		req.OnProgress(view.Progress{Reasoning: "working"})
	}
	var err error
	select {
	case err = <-call.finish:
	case <-ctx.Done():
		close(call.canceled)
		<-call.finish
		err = ctx.Err()
	}
	return turn.Result{Text: "answer: " + req.Input}, err
}

func nextCall(t *testing.T, h *queueHandler) *queueCall {
	t.Helper()
	select {
	case call := <-h.started:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("the queue did not start its next exchange")
		return nil
	}
}

func noCall(t *testing.T, h *queueHandler) {
	t.Helper()
	select {
	case call := <-h.started:
		t.Fatalf("started %q before the running exchange finished", call.req.Input)
	case <-time.After(30 * time.Millisecond):
	}
}

func enqueueForTest(t *testing.T, s *Service, conversation, input string, quotes ...QuoteRef) Exchange {
	t.Helper()
	e, err := s.Enqueue(context.Background(), conversation, input, quotes)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func awaitExchange(t *testing.T, s *Service, id string) Exchange {
	t.Helper()
	s.mu.Lock()
	var found *queuedExchange
	for _, list := range s.exchanges {
		for _, e := range list {
			if e.ID == id {
				found = e
			}
		}
	}
	s.mu.Unlock()
	if found == nil {
		t.Fatalf("exchange %s missing", id)
	}
	select {
	case <-found.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("exchange %s did not finish", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyExchange(found.Exchange)
}

func TestQueueDrainsWithoutAClientAndKeepsExchangeIDs(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	model := readmodel.New(readmodel.Sources{})
	events, stop := model.Subscribe(context.Background())
	defer stop()
	s := New(h, "owner", model)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	first, err := s.Enqueue(ctx, "main", "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	one := nextCall(t, h)
	cancel()
	second := enqueueForTest(t, s, "main", "second")
	if first.State != "running" || second.State != "queued" || first.ID == second.ID {
		t.Fatalf("exchanges: %+v %+v", first, second)
	}
	noCall(t, h)
	// Another conversation has its own turn reservation.
	other := enqueueForTest(t, s, "other", "independent")
	parallel := nextCall(t, h)
	if parallel.req.ConversationID != "console:other" {
		t.Fatal(parallel.req.ConversationID)
	}
	parallel.finish <- nil
	awaitExchange(t, s, other.ID)
	one.finish <- nil
	two := nextCall(t, h)
	if two.req.Input != "second" || !two.req.Queue {
		t.Fatalf("next = %+v", two.req)
	}
	two.finish <- nil
	for _, id := range []string{first.ID, second.ID} {
		if e := awaitExchange(t, s, id); e.State != "done" || e.ReplyID == "" {
			t.Fatalf("completed = %+v", e)
		}
	}
	lines := s.Replies("main")
	if len(lines) != 4 {
		t.Fatalf("lines = %+v", lines)
	}
	for i, id := range []string{first.ID, second.ID} {
		if lines[2*i].ID == "" || lines[2*i].ID == lines[2*i+1].ID || lines[2*i].ExchangeID != id || lines[2*i+1].ExchangeID != id {
			t.Fatalf("exchange IDs = %+v", lines)
		}
	}
	var saved transcript
	raw, _, _ := doc.Load()
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exchanges["console:main"][1].ReplyID != lines[3].ID {
		t.Fatal("reply and exchange were not saved together")
	}
	seen := map[string]map[string]bool{}
	for {
		select {
		case ev := <-events:
			if ev.Conversation != "console:main" || ev.Kind == "console.queue" {
				continue
			}
			if ev.ExchangeID == "" {
				t.Fatalf("missing event exchange ID: %+v", ev)
			}
			if seen[ev.ExchangeID] == nil {
				seen[ev.ExchangeID] = map[string]bool{}
			}
			seen[ev.ExchangeID][ev.Kind] = true
			if ev.Kind != "console.progress" && ev.ReplyID == "" {
				t.Fatalf("missing reply ID: %+v", ev)
			}
		default:
			for _, id := range []string{first.ID, second.ID} {
				for _, kind := range []string{"console.sent", "console.progress", "console.reply"} {
					if !seen[id][kind] {
						t.Fatalf("missing %s for %s: %+v", kind, id, seen)
					}
				}
			}
			return
		}
	}
}

func TestQueueDeleteAndEditPreserveOrderAndQuotes(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	quote := s.record(readmodel.Reply{Conversation: "console:main", Kind: "reply", Text: "quoted context"})
	first := enqueueForTest(t, s, "main", "first")
	one := nextCall(t, h)
	removed := enqueueForTest(t, s, "main", "remove")
	edited := enqueueForTest(t, s, "main", "draft", QuoteRef{Conversation: "main", ReplyID: quote.ID})
	if err := s.DeleteQueued(removed.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteQueued(removed.ID); !errors.Is(err, readmodel.ErrExchangeNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := s.DeleteQueued(first.ID); !errors.Is(err, readmodel.ErrExchangeNotQueued) {
		t.Fatalf("delete running: %v", err)
	}
	if _, err := s.EditQueued(edited.ID, " "); err == nil {
		t.Fatal("accepted empty edit")
	}
	e, err := s.EditQueued(edited.ID, "edited")
	if err != nil || e.ID != edited.ID || !e.EnqueuedAt.Equal(edited.EnqueuedAt) || len(e.Quotes) != 1 {
		t.Fatalf("edit = %+v %v", e, err)
	}
	// A projection cannot mutate stored quotes through a shared slice.
	list := s.Queue("main")
	list[1].Quotes[0].ReplyID = "changed by client"
	one.finish <- nil
	next := nextCall(t, h)
	if !strings.Contains(next.req.Input, "quoted context") || !strings.HasSuffix(next.req.Input, "edited") {
		t.Fatalf("edited prompt: %q", next.req.Input)
	}
	next.finish <- nil
	awaitExchange(t, s, e.ID)
	noCall(t, h)
}

func TestConcurrentEnqueuesReserveOnlyOneTurn(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 16)}
	s := New(h, "owner", nil)
	var submitted sync.WaitGroup
	errors := make(chan error, 16)
	for i := range 16 {
		submitted.Add(1)
		go func() {
			defer submitted.Done()
			_, err := s.Enqueue(context.Background(), "main", fmt.Sprintf("line %d", i), nil)
			errors <- err
		}()
	}
	submitted.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	list := s.Queue("main")
	if len(list) != 16 {
		t.Fatalf("accepted %d submissions", len(list))
	}
	for i, e := range list {
		call := nextCall(t, h)
		if i == 0 {
			noCall(t, h)
		}
		if call.req.Input != e.Input {
			t.Fatalf("order: %q, want %q", call.req.Input, e.Input)
		}
		call.finish <- nil
		awaitExchange(t, s, e.ID)
	}
}

func TestSteerInterruptsAndKeepsTheRemainingQueue(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	quote := s.record(readmodel.Reply{Conversation: "console:main", Kind: "reply", Text: "context"})
	first := enqueueForTest(t, s, "main", "first")
	one := nextCall(t, h)
	second := enqueueForTest(t, s, "main", "second")
	steer := enqueueForTest(t, s, "main", "correction", QuoteRef{Conversation: "main", ReplyID: quote.ID})
	tail := enqueueForTest(t, s, "main", "tail")
	e, err := s.Steer(context.Background(), steer.ID)
	if err != nil || e.State != "running" || e.ID != steer.ID {
		t.Fatalf("steer = %+v %v", e, err)
	}
	correction := nextCall(t, h)
	if correction.req.Queue || !strings.HasPrefix(correction.req.Input, "!") || !strings.HasSuffix(correction.req.Input, "correction") {
		t.Fatalf("interrupt prompt = %+v", correction.req)
	}
	select {
	case <-one.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn was not canceled")
	}
	if _, err := s.Steer(context.Background(), steer.ID); !errors.Is(err, readmodel.ErrExchangeNotQueued) {
		t.Fatalf("steer twice: %v", err)
	}
	correction.finish <- nil
	awaitExchange(t, s, steer.ID)
	noCall(t, h) // The canceled turn is still unwinding.
	one.finish <- nil
	if e := awaitExchange(t, s, first.ID); e.State != "failed" {
		t.Fatalf("canceled = %+v", e)
	}
	for _, e := range []Exchange{second, tail} {
		call := nextCall(t, h)
		if call.req.Input != e.Input {
			t.Fatalf("lost FIFO order: %q want %q", call.req.Input, e.Input)
		}
		call.finish <- nil
		awaitExchange(t, s, e.ID)
	}
}

func TestSendWaitsForItsExchangeAndCancelBypassesQueue(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	first := enqueueForTest(t, s, "main", "first")
	one := nextCall(t, h)
	answer := make(chan outcome, 1)
	go func() {
		r, err := s.SendCommand(context.Background(), "main", "second", "command")
		answer <- outcome{r, err}
	}()
	deadline := time.After(2 * time.Second)
	for len(s.Queue("main")) < 2 {
		select {
		case <-deadline:
			t.Fatal("send did not enqueue")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := s.SendCommand(context.Background(), "main", "second", "command"); !errors.Is(err, ErrCommandRunning) {
		t.Fatalf("duplicate: %v", err)
	}
	cancel := enqueueForTest(t, s, "main", "/cancel")
	stop := nextCall(t, h)
	if stop.req.Input != "/cancel" {
		t.Fatal("cancel waited behind the running turn")
	}
	stop.finish <- nil
	awaitExchange(t, s, cancel.ID)
	noCall(t, h)
	one.finish <- nil
	awaitExchange(t, s, first.ID)
	two := nextCall(t, h)
	two.finish <- nil
	select {
	case out := <-answer:
		if out.err != nil || out.reply.ID == "" || out.reply.ExchangeID == "" {
			t.Fatalf("send = %+v", out)
		}
		r, err := s.SendCommand(context.Background(), "main", "second", "command")
		if err != nil || r.ID != out.reply.ID || r.ExchangeID != out.reply.ExchangeID {
			t.Fatalf("retry = %+v %v", r, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return")
	}
	noCall(t, h)
}

func TestQueueRestoresWaitingWorkWithoutReplayingRunningTurn(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	first := enqueueForTest(t, s, "main", "already started")
	one := nextCall(t, h)
	removed := enqueueForTest(t, s, "main", "removed")
	waiting := enqueueForTest(t, s, "main", "draft")
	if err := s.DeleteQueued(removed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditQueued(waiting.ID, "saved edit"); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := doc.Load()
	// Release the original service after taking the crash snapshot.
	one.finish <- nil
	two := nextCall(t, h)
	two.finish <- nil
	awaitExchange(t, s, waiting.ID)
	restored := New(h, "owner", nil)
	if err := restored.Persist(&memDoc{raw: raw, saved: true}); err != nil {
		t.Fatal(err)
	}
	if err := restored.Drain(); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	if call.req.Input != "saved edit" {
		t.Fatalf("replayed wrong work: %q", call.req.Input)
	}
	list := restored.Queue("main")
	if len(list) != 2 || list[0].ID != first.ID || list[0].State != "failed" || list[1].ID != waiting.ID {
		t.Fatalf("restored = %+v", list)
	}
	if list[0].ReplyID == "" {
		t.Fatal("restart failure has no reply")
	}
	call.finish <- nil
	awaitExchange(t, restored, waiting.ID)
	noCall(t, h)
}

type brokenQueueDoc struct {
	memDoc
	muErr sync.Mutex
	err   error
}

func TestRestoredQueueStillRequiresAnOwner(t *testing.T) {
	h := &echo{}
	s := New(h, "", nil)
	doc := &memDoc{saved: true, raw: []byte(`{"replies":{},"exchanges":{"console:main":[{"id":"e1","conversation":"console:main","input":"hello","state":"queued"}]}}`)}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	if e := awaitExchange(t, s, "e1"); e.State != "failed" || len(h.seen) != 0 {
		t.Fatalf("restored work acted without an owner: %+v", e)
	}
}

func (d *brokenQueueDoc) Save(raw []byte) error {
	d.muErr.Lock()
	err := d.err
	d.muErr.Unlock()
	if err != nil {
		return err
	}
	return d.memDoc.Save(raw)
}

func TestQueueDoesNotAcceptOrMutateUndurableWork(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	doc := &brokenQueueDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	first := enqueueForTest(t, s, "main", "first")
	one := nextCall(t, h)
	waiting := enqueueForTest(t, s, "main", "waiting")
	doc.muErr.Lock()
	doc.err = errors.New("disk unavailable")
	doc.muErr.Unlock()
	if _, err := s.Enqueue(context.Background(), "other", "undurable", nil); err == nil {
		t.Fatal("accepted undurable submission")
	}
	if err := s.DeleteQueued(waiting.ID); err == nil {
		t.Fatal("deleted undurable submission")
	}
	if _, err := s.EditQueued(waiting.ID, "changed"); err == nil {
		t.Fatal("edited undurable submission")
	}
	if _, err := s.Steer(context.Background(), waiting.ID); err == nil {
		t.Fatal("started undurable steer")
	}
	list := s.Queue("main")
	if len(list) != 2 || list[1].Input != "waiting" || list[1].State != "queued" || len(s.Queue("other")) != 0 {
		t.Fatalf("mutation did not roll back: %+v", list)
	}
	noCall(t, h)
	doc.muErr.Lock()
	doc.err = nil
	doc.muErr.Unlock()
	one.finish <- nil
	two := nextCall(t, h)
	two.finish <- nil
	awaitExchange(t, s, first.ID)
	awaitExchange(t, s, waiting.ID)
}
