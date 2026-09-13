package console

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/task"
)

func TestContinuationReceiptWaitsForParentAndRetriesOnlyUnadmittedWork(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 4)}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	submit := func(s *Service) error {
		return s.ContinueTask(t.Context(), "main", "parent", "deliver:child", "worker", "child finished", "the complete child answer")
	}
	if err := submit(s); !errors.Is(err, channel.ErrDeliveryQueued) {
		t.Fatalf("queue acceptance reported delivery: %v", err)
	}
	first := nextCall(t, h)
	if first.req.ExpectedTask != "parent" {
		t.Fatal("lost parent binding")
	}
	id := s.Queue("main")[0].ID
	// The parent was paused after dispatch preparation, before task admission.
	first.finish <- task.ErrContinuationUnavailable
	if e := awaitExchange(t, s, id); e.State != consoleapi.ExchangeFailed {
		t.Fatal("rejection not recorded")
	}
	// Reload the failed receipt before allowing the same continuation to run.
	restored := New(h, "owner", nil)
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if err := submit(restored); !errors.Is(err, channel.ErrDeliveryQueued) {
		t.Fatalf("unadmitted continuation could not retry: %v", err)
	}
	second := nextCall(t, h)
	if second.req.MessageID != first.req.MessageID || second.req.ExpectedTask != "parent" || second.req.Input != first.req.Input {
		t.Fatal("retry changed identity or lost the child answer")
	}
	if found, err := restored.ContinuationReceipt("main", "parent", "deliver:child"); !found || !errors.Is(err, channel.ErrDeliveryQueued) {
		t.Fatalf("running receipt = %v, %v", found, err)
	}
	second.finish <- nil
	awaitExchange(t, restored, id)
	for range 3 {
		if err := submit(restored); err != nil {
			t.Fatal(err)
		}
	}
	if err := restored.ContinueTask(t.Context(), "main", "parent", "deliver:child", "worker", "new landing notice", "changed landing description"); err != nil {
		t.Fatalf("receipt depended on rebuilding an already accepted message: %v", err)
	}
	noCall(t, h)
	if len(restored.Queue("main")) != 1 {
		t.Fatal("retry duplicated the durable exchange")
	}
}

func TestProjectTransferRetainsRejectedContinuationAdmission(t *testing.T) {
	source := &memDoc{}
	reply := consoleapi.Reply{ID: "r1", Conversation: "console:source", ProjectID: "p", ExchangeID: "e1", Kind: "reply", Error: "parent paused"}
	saved := transcript{Replies: map[string][]consoleapi.Reply{"console:source": {reply}}, Exchanges: map[string][]*queuedExchange{"console:source": {{Exchange: Exchange{ID: "e1", Conversation: "console:source", ExpectedProject: "p", ExpectedTask: "parent", Key: "deliver:child", Input: "child finished", Prompt: "@worker complete original answer", State: consoleapi.ExchangeFailed}, ContinuationRejected: true, Receipt: &reply}}}}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Save(raw); err != nil {
		t.Fatal(err)
	}
	bundle, err := ExportProject(source, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err = bundle.Remap(func(id string) string { return "origin~" + id }, func(string) string { return "console:imported" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	dest := &memDoc{}
	if err := ImportProject(dest, bundle); err != nil {
		t.Fatal(err)
	}
	h := &queueHandler{started: make(chan *queueCall, 1)}
	s := New(h, "owner", nil)
	if err := s.Persist(dest); err != nil {
		t.Fatal(err)
	}
	e := bundle.Exchanges["console:imported"][0]
	if err := s.ContinueTask(t.Context(), "console:imported", "origin~parent", e.Key, "worker", "new notice", "must retain original"); !errors.Is(err, channel.ErrDeliveryQueued) {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	if call.req.ExpectedTask != "origin~parent" || call.req.Input != "@worker complete original answer" {
		t.Fatalf("migration lost input or binding: %+v", call.req)
	}
	call.finish <- nil
	awaitExchange(t, s, e.ID)
}

func TestContinuationNeverReplaysAnAdmittedFailure(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 2)}
	s := New(h, "owner", nil)
	submit := func() error {
		return s.ContinueTask(t.Context(), "main", "parent", "deliver:child", "worker", "notice", "answer")
	}
	if err := submit(); !errors.Is(err, channel.ErrDeliveryQueued) {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	call.finish <- errors.New("execution lost after model input")
	awaitExchange(t, s, s.Queue("main")[0].ID)
	if err := submit(); !errors.Is(err, channel.ErrOutcomeUnknown) {
		t.Fatalf("admitted failure was replayable: %v", err)
	}
	noCall(t, h)
}

func TestRecoveredContinuationRequestKeepsExpectedTask(t *testing.T) {
	s := New(nil, "owner", nil)
	r := exchangeRecovery{s: s, exchange: Exchange{ID: "e", Conversation: "console:c", ExpectedTask: "original-parent", ExpectedProject: "p"}}
	req := r.request("owner", &questionIdentity{})
	if req.ExpectedTask != "original-parent" {
		t.Fatal("restart dropped parent task binding")
	}
}

func TestContinuationDoesNotConfirmAnUnsavedTerminalReply(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 2)}
	s := New(h, "owner", nil)
	doc := &brokenQueueDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	if err := s.ContinueTask(t.Context(), "main", "parent", "deliver:child", "worker", "notice", "answer"); !errors.Is(err, channel.ErrDeliveryQueued) {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	doc.muErr.Lock()
	doc.err = errors.New("disk unavailable")
	doc.muErr.Unlock()
	defer func() {
		doc.muErr.Lock()
		doc.err = nil
		doc.muErr.Unlock()
		s.workers.Wait()
	}()
	call.finish <- nil
	deadline := time.Now().Add(2 * time.Second)
	for s.Queue("main")[0].State != consoleapi.ExchangeDone {
		if time.Now().After(deadline) {
			t.Fatal("handler did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if found, err := s.ContinuationReceipt("main", "parent", "deliver:child"); !found || !errors.Is(err, channel.ErrDeliveryQueued) {
		t.Fatalf("unsaved terminal reply confirmed: %v, %v", found, err)
	}
	if err := s.ContinueTask(t.Context(), "main", "parent", "deliver:child", "worker", "notice", "answer"); !errors.Is(err, channel.ErrDeliveryQueued) {
		t.Fatalf("unsaved terminal reply confirmed by retry: %v", err)
	}
}
