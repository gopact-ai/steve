package console

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type recoveryDriver struct {
	resume     func(context.Context, string, turn.Request) (turn.Result, error)
	calls      atomic.Int32
	candidates []turn.RetainedChat
}

func (d *recoveryDriver) RetainedChats(context.Context) ([]turn.RetainedChat, error) {
	if d.candidates != nil {
		return d.candidates, nil
	}
	return []turn.RetainedChat{{AttemptID: "attempt-1", TaskID: "task-1", Conversation: "console:main", MessageID: "web-e1", AgentID: "worker", NodeID: "node-a"}}, nil
}

func TestManagedObserverDetachmentReconcilesSameExchangeWhileCoordinatorStaysOnline(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 1)}
	s := New(h, "owner", nil)
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(&memDoc{}); err != nil {
		t.Fatal(err)
	}
	driver := &recoveryDriver{resume: func(_ context.Context, id string, req turn.Request) (turn.Result, error) {
		return turn.Result{Text: "original retained result", Attempt: id}, nil
	}}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	e := enqueueForTest(t, s, "main", "original prompt")
	call := nextCall(t, h)
	driver.candidates = []turn.RetainedChat{{AttemptID: "original-attempt", TaskID: "original-task", Conversation: e.Conversation, MessageID: AnchorMark + e.ID}}
	call.finish <- harness.ErrStopUnconfirmed
	if got := awaitExchange(t, s, e.ID); got.State != "done" {
		t.Fatalf("observer detachment terminalized original execution: %+v", got)
	}
	if driver.calls.Load() != 1 {
		t.Fatalf("recovery invoked %d times", driver.calls.Load())
	}
	noCall(t, h)
}

func TestPlatformRecoveryCannotForgeNativePermissionOrOwner(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.RequestRecovery(t.Context(), consoleapi.PendingQuestion{Conversation: "console:main", ExchangeID: "e1", TaskID: "child-task", AttemptID: "child-attempt", Principal: "forged", ToolCallID: "forged-native"}, view.Question{Kind: "permission", Message: "检查了原节点，网络不可达。请恢复后重试。", Choices: []view.Choice{{Value: "retry", Label: "重试"}}})
		done <- err
	}()
	var question consoleapi.PendingQuestion
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		list := s.Questions("main")
		if len(list) > 0 {
			question = list[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if question.ID == "" || question.Kind != "recovery" || question.Principal != "owner" || question.ToolCallID != "" || !question.Deadline.IsZero() {
		t.Fatalf("platform recovery forged native authority: %+v", question)
	}
	if _, err := s.AnswerQuestion(t.Context(), question.ID, consoleapi.QuestionAnswer{CommandID: "retry-platform", Decision: "accept", Choice: "retry"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func (d *recoveryDriver) ResumeRetainedChat(ctx context.Context, id string, req turn.Request) (turn.Result, error) {
	d.calls.Add(1)
	return d.resume(ctx, id, req)
}

func recoveryDocument() *memDoc {
	return &memDoc{saved: true, raw: []byte(`{"replies":{"console:main":[{"id":"sent-1","conversation":"console:main","exchange_id":"e1","kind":"sent","input":"original"}]},"exchanges":{"console:main":[{"id":"e1","conversation":"console:main","input":"original","key":"client:original","state":"running"},{"id":"e2","conversation":"console:main","input":"follow-up","state":"queued"}]}}`)}
}

func TestRecoveryUsesOriginalExchangeAndDoesNotReplayItsPrompt(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 3)}
	s := New(h, "owner", nil)
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	if list := s.Queue("main"); len(list) != 2 || list[0].State != "recovering" || list[0].ReplyID != "" {
		t.Fatalf("lost recovery identity: %+v", list)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	driver := &recoveryDriver{resume: func(ctx context.Context, id string, req turn.Request) (turn.Result, error) {
		if id != "attempt-1" || req.MessageID != "web-e1" || req.ConversationID != "console:main" {
			t.Errorf("recovery changed identity: %+v", req)
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return turn.Result{}, ctx.Err()
		}
		return turn.Result{Text: "completed on retained node", Attempt: id}, nil
	}}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-h.started:
		t.Fatalf("replayed running prompt or overtook it: %s", call.req.Input)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	first := awaitExchange(t, s, "e1")
	if first.State != "done" || first.ReplyID == "" {
		t.Fatalf("recovery did not finish original exchange: %+v", first)
	}
	follow := nextCall(t, h)
	if follow.req.Input != "follow-up" {
		t.Fatalf("wrong queue item: %s", follow.req.Input)
	}
	follow.finish <- nil
	awaitExchange(t, s, "e2")
	if driver.calls.Load() != 1 {
		t.Fatal("duplicate attachment")
	}
	sent := 0
	for _, r := range s.Replies("main") {
		if r.ExchangeID == "e1" && r.Kind == "sent" {
			sent++
		}
	}
	if sent != 1 {
		t.Fatalf("original input was duplicated %d times", sent)
	}
}

func TestUnverifiedRecoveryPersistsAskUserThenRechecksWithoutNewPrompt(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 3)}
	s := New(h, "owner", nil)
	s.EnableRetainedRecovery(t.Context())
	doc := recoveryDocument()
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	driver := &recoveryDriver{resume: func(ctx context.Context, id string, req turn.Request) (turn.Result, error) {
		if !ready.Load() {
			return turn.Result{}, &turn.RecoveryBlocked{Question: view.Question{Kind: "recovery", Title: "需要处理", Message: "已检查原执行。节点暂时无法连接，请恢复后重新检查。", Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: "重新检查"}}}, Cause: errors.New("node disconnected")}
		}
		return turn.Result{Text: "retained result", Attempt: id}, nil
	}}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	var id string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		for _, q := range s.Questions("main") {
			if q.State == "pending" {
				id = q.ID
				if !q.Deadline.IsZero() || q.TaskID != "task-1" || q.AttemptID != "attempt-1" {
					t.Fatalf("invalid recovery question: %+v", q)
				}
			}
		}
		if id != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("missing persistent Ask User")
	}
	ready.Store(true)
	if _, err := s.AnswerQuestion(t.Context(), id, consoleapi.QuestionAnswer{CommandID: "retry-1", Decision: "accept", Choice: "retry"}); err != nil {
		t.Fatal(err)
	}
	if got := awaitExchange(t, s, "e1"); got.State != "done" {
		t.Fatalf("retry did not reconcile original execution: %+v", got)
	}
	follow := nextCall(t, h)
	follow.finish <- nil
	awaitExchange(t, s, "e2")
}

func TestRetainedGenerationShutdownStopsFailedSaveRetriesWithoutTerminalizing(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 1)}
	lifetime, cancelLifetime := context.WithCancel(t.Context())
	defer cancelLifetime()
	s := New(h, "owner", nil)
	s.EnableRetainedRecovery(lifetime)
	doc := &brokenQueueDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	e := enqueueForTest(t, s, "main", "original prompt")
	call := nextCall(t, h)
	doc.muErr.Lock()
	doc.err = errors.New("old generation cannot write")
	doc.muErr.Unlock()
	call.finish <- nil
	// A failed final save must stop retrying once this generation ends.
	cancelLifetime()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("old generation retained a never-writable worker: %v", err)
	}
	raw, _, err := doc.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.muErr.Lock()
	doc.err = nil
	doc.muErr.Unlock()
	restarted := New(h, "owner", nil)
	restarted.EnableRetainedRecovery(t.Context())
	if err := restarted.Persist(&memDoc{saved: true, raw: raw}); err != nil {
		t.Fatal(err)
	}
	for _, item := range restarted.Queue("main") {
		if item.ID == e.ID && (item.State != "recovering" || item.ReplyID != "") {
			t.Fatalf("detachment invented a terminal reply: %+v", item)
		}
	}
}

// unwritableRecoveringDoc refuses the save that records an exchange as
// recovering and takes every other write, so a test can lose exactly
// that one state write.
type unwritableRecoveringDoc struct {
	*memDoc
	armed atomic.Bool
}

func (d *unwritableRecoveringDoc) Save(raw []byte) error {
	if d.armed.Load() && bytes.Contains(raw, []byte(`"state":"recovering"`)) {
		return errors.New("transcript volume is full")
	}
	return d.memDoc.Save(raw)
}

// A recovery that cannot record the exchange as recovering must not
// resume the retained execution behind an unwritten state: it holds the
// exchange for the owner and puts the block to them.
func TestRecoveryAsksTheOwnerWhenRecordingRecoveringFails(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := New(&echo{}, "owner", nil)
	s.EnableRetainedRecovery(lifetime)
	doc := &unwritableRecoveringDoc{memDoc: recoveryDocument()}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	driver := &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		return turn.Result{Text: "resumed", Attempt: "attempt-1"}, nil
	}}
	doc.armed.Store(true)
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	question := awaitRecoveryQuestion(t, s)
	if question.Title != "原执行需要核实" {
		t.Fatalf("unsaved recovering state asked another question: %+v", question)
	}
	if driver.calls.Load() != 0 {
		t.Fatalf("resumed behind an unwritten state: %d", driver.calls.Load())
	}
	if got := s.Queue("main"); got[0].State != consoleapi.ExchangeAwaitingUser {
		t.Fatalf("exchange not held for the owner: %+v", got)
	}
}

func awaitRecoveryQuestion(t *testing.T, s *Service) consoleapi.PendingQuestion {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		for _, q := range s.Questions("main") {
			if q.State == "pending" {
				return q
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("recovery question not opened")
	return consoleapi.PendingQuestion{}
}
