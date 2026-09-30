package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type stoppingRecoveryDriver struct {
	*recoveryDriver
	stops    atomic.Int32
	stopErr  error
	once     sync.Once
	stopHook func()
}

func (d *stoppingRecoveryDriver) StopRetainedTask(_ context.Context, id string, req turn.Request, cancel bool) (turn.Result, error) {
	d.stops.Add(1)
	if d.stopHook != nil {
		d.once.Do(d.stopHook)
	}
	if id != "task-1" || req.ConversationID != "console:main" || req.MessageID != "web-e1" || req.SenderOpenID != "owner" {
		return turn.Result{}, errors.New("stop targeted another execution")
	}
	return turn.Result{Text: "original task stopped", Attempt: id}, d.stopErr
}
func waitRecoveryQuestion(t *testing.T, s *Service) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		for _, q := range s.Questions("main") {
			if q.State == "pending" {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("recovery question not opened")
}
func TestStopRecoveryTargetsOriginalTaskAndPersistsSettlement(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "uncertain"}[uncertain], func(t *testing.T) {
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			doc := recoveryDocument()
			s := impatient(New(&echo{}, "owner", nil))
			s.EnableRetainedRecovery(lifetime)
			if err := s.Persist(doc); err != nil {
				t.Fatal(err)
			}
			driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
				return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "Open was not acknowledged. Check the original machine.", Choices: []view.Choice{{Value: "retry", Label: "Retry"}}}}
			}}}
			if uncertain {
				driver.stopErr = harness.ErrStopUnconfirmed
			}
			if err := s.RecoverChats(lifetime, driver); err != nil {
				t.Fatal(err)
			}
			waitRecoveryQuestion(t, s)
			reply, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-original")
			if driver.stops.Load() != 1 {
				t.Fatalf("Stop did not target original retained task: calls=%d reply=%+v err=%v", driver.stops.Load(), reply, err)
			}
			if uncertain {
				if !errors.Is(err, harness.ErrStopUnconfirmed) && !strings.Contains(reply.Error, harness.ErrStopUnconfirmed.Error()) {
					t.Fatalf("uncertain stop reported success: %+v %v", reply, err)
				}
				if s.Queue("main")[0].State != "awaiting-user" {
					t.Fatal("uncertain execution terminalized")
				}
				waitRecoveryQuestion(t, s)
				if s.Queue("main")[1].State != "queued" {
					t.Fatal("uncertain stop released queue")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := awaitExchange(t, s, "e1"); !got.State.Terminal() {
					t.Fatalf("original exchange not settled: %+v", got)
				}
				for _, q := range s.Questions("main") {
					if q.State == "pending" {
						t.Fatal("stopped task kept pending question")
					}
				}
				awaitExchange(t, s, "e2")
			}
			_, _ = s.SendCommand(t.Context(), "main", "/cancel", "stop-original")
			if uncertain && driver.stops.Load() < 2 {
				t.Fatal("unconfirmed stop was not checked again")
			}
			if !uncertain && driver.stops.Load() != 1 {
				t.Fatal("confirmed stop contacted the original execution again")
			}
			cancel()
			s.workers.Wait()
			restored := impatient(New(&echo{}, "owner", nil))
			restored.EnableRetainedRecovery(t.Context())
			if err := restored.Persist(doc); err != nil {
				t.Fatal(err)
			}
			got := restored.Queue("main")[0]
			if uncertain && got.State.Terminal() {
				t.Fatal("restart lost uncertainty")
			}
			if !uncertain && !got.State.Terminal() {
				t.Fatal("restart resurrected stopped task")
			}
		})
	}
}

func TestRestartAfterConfirmedRecoveryStopDoesNotContactExecutorAgain(t *testing.T) {
	doc := recoveryDocument()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the confirmed stop receipt committed, before the
	// waiting recovery goroutine had written the final transcript projection.
	s.mu.Lock()
	s.exchanges["console:main"][0].RecoveryStop = &consoleapi.Reply{Text: "original task stopped"}
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restored := impatient(New(&echo{}, "owner", nil))
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	driver := &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		t.Error("confirmed stop restarted native observation")
		return turn.Result{}, nil
	}}
	if err := restored.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	if got := awaitExchange(t, restored, "e1"); got.State != "cancelled" {
		t.Fatalf("stop receipt not delivered: %+v", got)
	}
	awaitExchange(t, restored, "e2")
	if driver.calls.Load() != 0 {
		t.Fatal("confirmed stop contacted executor again")
	}
}

func TestStopDetachedRecoverySettlesWithoutClosingWaiterTwice(t *testing.T) {
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{}}
	s.mu.Lock()
	s.recoveryDriver = driver
	e := s.exchanges["console:main"][0]
	s.detachRecoveryLocked(e, errors.New("observer stopped after a failed save"))
	s.mu.Unlock()
	if _, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-detached"); err != nil {
		t.Fatal(err)
	}
	if got := awaitExchange(t, s, "e1"); got.State != "cancelled" {
		t.Fatalf("detached recovery not stopped: %+v", got)
	}
	awaitExchange(t, s, "e2")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running["console:main"] != 0 {
		t.Fatalf("reservation count=%d", s.running["console:main"])
	}
}

func TestConfirmedRecoveryStopWinsObserverCancellationReply(t *testing.T) {
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(ctx context.Context, id string, _ turn.Request) (turn.Result, error) {
		close(started)
		<-ctx.Done()
		return turn.Result{Text: "observer cancelled", Attempt: id}, ctx.Err()
	}}}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-observing"); err != nil {
		t.Fatal(err)
	}
	got := awaitExchange(t, s, "e1")
	if got.State != "cancelled" {
		t.Fatalf("observer error replaced confirmed stop: %+v", got)
	}
	for _, reply := range s.Replies("main") {
		if reply.ID == got.ReplyID && (reply.Error != "" || reply.Text != "original task stopped") {
			t.Fatalf("wrong final reply: %+v", reply)
		}
	}
	awaitExchange(t, s, "e2")
}

func TestRecoveryStopWaitsForObserverBeforePublishingConfirmedReceipt(t *testing.T) {
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		close(started)
		<-release
		close(returned)
		return turn.Result{Attempt: "attempt-1", Text: "observer cancelled"}, context.Canceled
	}}, stopHook: func() { close(release); <-returned; time.Sleep(30 * time.Millisecond) }}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-before-receipt"); err != nil {
		t.Fatal(err)
	}
	got := awaitExchange(t, s, "e1")
	if got.State != "cancelled" {
		t.Fatalf("observer finished before stop receipt: %+v", got)
	}
	for _, reply := range s.Replies("main") {
		if reply.ID == got.ReplyID && (reply.Error != "" || reply.Text != "original task stopped") {
			t.Fatalf("confirmed receipt lost: %+v", reply)
		}
	}
	awaitExchange(t, s, "e2")
}

func TestUnconfirmedChildStopKeepsFinishedParentExchangeReserved(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	doc := recoveryDocument()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(lifetime)
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		close(started)
		<-release
		close(returned)
		return turn.Result{Attempt: "attempt-1"}, context.Canceled
	}}, stopHook: func() { close(release); <-returned; time.Sleep(30 * time.Millisecond) }, stopErr: harness.ErrStopUnconfirmed}
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-child-unknown"); err == nil {
		t.Fatal("uncertain child reported stopped")
	}
	waitRecoveryQuestion(t, s)
	if got := s.Queue("main"); got[0].State != "awaiting-user" || got[1].State != "queued" {
		t.Fatalf("uncertain child released parent: %+v", got)
	}
	cancel()
	s.workers.Wait()
	restored := impatient(New(&echo{}, "owner", nil))
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	restartDriver := &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		t.Error("pending task stop resumed observer")
		return turn.Result{}, nil
	}}
	if err := restored.RecoverChats(t.Context(), restartDriver); err != nil {
		t.Fatal(err)
	}
	waitRecoveryQuestion(t, restored)
	if got := restored.Queue("main"); got[0].State != "awaiting-user" || got[1].State != "queued" {
		t.Fatalf("restart lost child stop uncertainty: %+v", got)
	}
}

// The stop a console reports as unconfirmed is often confirmed moments
// later by the durable task-stop reconciliation running behind it. The
// exchange must notice that on its own: parking on a card the owner can
// only read leaves the conversation running forever with nothing to do.
func TestUnconfirmedStopSettlesOnceARecheckConfirmsIt(t *testing.T) {
	doc := recoveryDocument()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	// The state a crash leaves behind: a stop was asked for and its
	// confirmation never arrived before the process went away.
	s.mu.Lock()
	s.exchanges["console:main"][0].RecoveryStopPending = "attempt att-1 writer is quarantined until physically confirmed stopped"
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restored := impatient(New(&echo{}, "owner", nil))
	restored.recoveryStopEvery = 20 * time.Millisecond
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		t.Error("a pending stop resumed the original observer")
		return turn.Result{}, nil
	}}, stopErr: harness.ErrStopUnconfirmed}
	if err := restored.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	// While it stays unconfirmed the owner is told, and the queue behind it
	// is still held for the original task.
	waitRecoveryQuestion(t, restored)
	if got := restored.Queue("main"); got[0].State != "awaiting-user" || got[1].State != "queued" {
		t.Fatalf("pending stop released the queue: %+v", got)
	}
	driver.stopErr = nil
	if got := awaitExchange(t, restored, "e1"); got.State != "cancelled" {
		t.Fatalf("a confirmed recheck did not settle the exchange: %+v", got)
	}
	for _, q := range restored.Questions("main") {
		if q.State == "pending" {
			t.Fatal("settled stop left its question pending")
		}
	}
	awaitExchange(t, restored, "e2")
}

func TestDetachedUnconfirmedRecoveryDoesNotReleaseQueuedInstructions(t *testing.T) {
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{}, stopErr: harness.ErrStopUnconfirmed}
	s.mu.Lock()
	s.recoveryDriver = driver
	e := s.exchanges["console:main"][0]
	s.detachRecoveryLocked(e, errors.New("observer save failed"))
	s.mu.Unlock()
	if _, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-detached-unknown"); err == nil {
		t.Fatal("unknown stop reported success")
	}
	if got := s.Queue("main"); got[0].State.Terminal() || got[1].State != "queued" {
		t.Fatalf("detached uncertainty released queue: %+v", got)
	}
}

func TestRecoveryStopIntentIsDurableBeforeStoppingExecutions(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	doc := recoveryDocument()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(lifetime)
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	var snapshot *memDoc
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "open not acknowledged", Choices: []view.Choice{{Value: "retry", Label: "Retry"}}}}
	}}, stopErr: harness.ErrStopUnconfirmed, stopHook: func() {
		raw, ok, err := doc.Load()
		if err != nil || !ok {
			t.Error("missing stop intent")
		}
		snapshot = &memDoc{saved: true, raw: raw}
	}}
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	waitRecoveryQuestion(t, s)
	_, _ = s.SendCommand(t.Context(), "main", "/cancel", "stop-crash-window")
	cancel()
	s.workers.Wait()
	restored := impatient(New(&echo{}, "owner", nil))
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.Persist(snapshot); err != nil {
		t.Fatal(err)
	}
	rd := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		t.Error("stop intent lost before result was committed")
		return turn.Result{Attempt: "attempt-1"}, nil
	}}}
	if err := restored.RecoverChats(t.Context(), rd); err != nil {
		t.Fatal(err)
	}
	// The durable owner intent is retried without another user command.
	// A successful stop may settle before a waiting card is ever rendered.
	awaitStops(t, &rd.stops)
	if got := awaitExchange(t, restored, "e1"); got.State != "cancelled" {
		t.Fatalf("stop could not complete after restart: %+v", got)
	}
	awaitExchange(t, restored, "e2")
}

func TestNewInputBehindDetachedRecoveryIsDurable(t *testing.T) {
	doc := recoveryDocument()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.detachRecoveryLocked(s.exchanges["console:main"][0], errors.New("observer failed"))
	s.mu.Unlock()
	added, err := s.Enqueue(t.Context(), "main", "preserve this later instruction", nil)
	if err != nil {
		t.Fatal(err)
	}
	restored := impatient(New(&echo{}, "owner", nil))
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range restored.Queue("main") {
		if e.ID == added.ID {
			found = true
			if e.State != "queued" {
				t.Fatalf("new instruction started prematurely: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("accepted queued instruction was not persisted")
	}
}

func TestRestartDoesNotTreatNewlyAcceptedStopAsNativeExecution(t *testing.T) {
	doc := &memDoc{saved: true, raw: []byte(`{"replies":{"console:main":[]},"exchanges":{"console:main":[{"id":"e1","conversation":"console:main","input":"original","state":"running"},{"id":"stop","conversation":"console:main","input":"/cancel","state":"running"}]}}`)}
	restored := impatient(New(&echo{}, "owner", nil))
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	got := restored.Queue("main")
	if got[0].State != "recovering" || got[1].State != "failed" {
		t.Fatalf("stop became a native recovery: %+v", got)
	}
}

func TestSameStopCommandRechecksOriginalTaskAfterNodeReturns(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	doc := recoveryDocument()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(lifetime)
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "node is offline", Choices: []view.Choice{{Value: "wait", Label: "Wait"}}}}
	}}, stopErr: harness.ErrStopUnconfirmed}
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	waitRecoveryQuestion(t, s)
	failed, err := s.SendCommand(t.Context(), "main", "/cancel", "same-stop")
	if err == nil {
		t.Fatal("offline stop reported success")
	}
	driver.stopErr = nil
	confirmed, err := s.SendCommand(t.Context(), "main", "/cancel", "same-stop")
	if err != nil {
		t.Fatalf("retry returned cached outage after original node confirmed stop: %v", err)
	}
	if confirmed.ExchangeID != failed.ExchangeID || driver.stops.Load() != 2 {
		t.Fatalf("retry changed stop identity or did not recheck: %+v calls=%d", confirmed, driver.stops.Load())
	}
	if got := awaitExchange(t, s, "e1"); got.State != "cancelled" {
		t.Fatalf("original task remains blocked: %+v", got)
	}
	for _, q := range s.Questions("main") {
		if q.State == "pending" {
			t.Fatal("stop confirmation left a question pending")
		}
	}
	awaitExchange(t, s, "e2")
	if cached, err := s.SendCommand(t.Context(), "main", "/cancel", "same-stop"); err != nil || cached.ID != confirmed.ID {
		t.Fatalf("confirmed stop was not replayed as its original receipt: %+v %v", cached, err)
	}
	if driver.stops.Load() != 2 {
		t.Fatal("confirmed stop was executed again")
	}
}

func TestRecoveryStopRetryKeepsItsTargetAcrossRestart(t *testing.T) {
	for _, oldReceipt := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "older-receipt"}[oldReceipt], func(t *testing.T) {
			lifetime, cancel := context.WithCancel(t.Context())
			doc := recoveryDocument()
			s := impatient(New(&echo{}, "owner", nil))
			s.EnableRetainedRecovery(lifetime)
			if err := s.Persist(doc); err != nil {
				t.Fatal(err)
			}
			driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
				return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "node is offline", Choices: []view.Choice{{Value: "wait", Label: "Wait"}}}}
			}}, stopErr: errors.New("attempt attempt-1 writer is quarantined until physically confirmed stopped")}
			if err := s.RecoverChats(lifetime, driver); err != nil {
				t.Fatal(err)
			}
			waitRecoveryQuestion(t, s)
			failed, err := s.SendCommand(t.Context(), "main", "/cancel", "restart-stop")
			if err == nil {
				t.Fatal("offline stop reported success")
			}
			cancel()
			s.workers.Wait()
			if oldReceipt {
				s.mu.Lock()
				for _, e := range s.exchanges["console:main"] {
					e.RecoveryCancelPending = false
					if e.ID == failed.ExchangeID {
						e.RecoveryStopTarget = nil
					}
				}
				err = s.save()
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			restored := impatient(New(&echo{}, "owner", nil))
			restored.EnableRetainedRecovery(t.Context())
			if err := restored.Persist(doc); err != nil {
				t.Fatal(err)
			}
			driver.stopErr = nil
			if err := restored.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			if oldReceipt {
				waitRecoveryQuestion(t, restored)
			} else {
				awaitExchange(t, restored, "e1")
			}
			if _, err := restored.SendSubmission(t.Context(), consoleapi.Submission{Conversation: "main", Input: "/cancel", CommandID: "restart-stop"}); err != nil {
				t.Fatalf("original stop failed after restart: %v", err)
			}
			if got := awaitExchange(t, restored, "e1"); got.State != "cancelled" {
				t.Fatalf("wrong stop target after restart: %+v", got)
			}
			awaitExchange(t, restored, "e2")
			if driver.stops.Load() != 2 {
				t.Fatalf("wrong number of checks: %d", driver.stops.Load())
			}
		})
	}
}

func TestConcurrentRetrySharesOriginalStopCheck(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(lifetime)
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Message: "offline", Choices: []view.Choice{{Value: "wait", Label: "Wait"}}}}
	}}, stopErr: harness.ErrStopUnconfirmed}
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	waitRecoveryQuestion(t, s)
	_, _ = s.SendCommand(t.Context(), "main", "/cancel", "parallel-stop")
	entered, release := make(chan struct{}), make(chan struct{})
	driver.stopErr = nil
	driver.stopHook = func() { close(entered); <-release }
	results := make(chan error, 2)
	go func() { _, err := s.SendCommand(t.Context(), "main", "/cancel", "parallel-stop"); results <- err }()
	<-entered
	go func() { _, err := s.SendCommand(t.Context(), "main", "/cancel", "parallel-stop"); results <- err }()
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if driver.stops.Load() != 2 {
		t.Fatalf("concurrent retry duplicated check: %d", driver.stops.Load())
	}
	awaitExchange(t, s, "e2")
}

type lookupFailureStopDriver struct {
	*stoppingRecoveryDriver
	lookupErr atomic.Bool
}

func (d *lookupFailureStopDriver) RetainedChatsFor(ctx context.Context, conversation, messageID string) ([]turn.RetainedChat, error) {
	if d.lookupErr.Load() {
		return nil, errors.New("retained lookup unavailable")
	}
	return d.recoveryDriver.RetainedChatsFor(ctx, conversation, messageID)
}

func TestSameStopRetryRecoversAfterOriginalTaskLookupFails(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(lifetime)
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	driver := &lookupFailureStopDriver{stoppingRecoveryDriver: &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "offline", Choices: []view.Choice{{Value: "wait", Label: "Wait"}}}}
	}}}}
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	waitRecoveryQuestion(t, s)
	driver.lookupErr.Store(true)
	if _, err := s.SendCommand(t.Context(), "main", "/cancel", "lookup-stop"); err == nil {
		t.Fatal("unavailable lookup reported success")
	}
	driver.lookupErr.Store(false)
	if _, err := s.SendCommand(t.Context(), "main", "/cancel", "lookup-stop"); err != nil {
		t.Fatalf("lookup recovery was cached as failed: %v", err)
	}
	if driver.stops.Load() != 1 {
		t.Fatalf("unexpected stop target calls: %d", driver.stops.Load())
	}
	awaitExchange(t, s, "e2")
}

func TestHistoricalStopDoesNotInferAnUnrelatedRecoveryTarget(t *testing.T) {
	for _, mode := range []string{"missing-attempt", "different-error", "newer-task"} {
		t.Run(mode, func(t *testing.T) {
			s := impatient(New(&echo{}, "owner", nil))
			s.EnableRetainedRecovery(t.Context())
			if err := s.Persist(recoveryDocument()); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			target := s.exchanges["console:main"][0]
			target.RecoveryStopPending = "attempt original-attempt writer is quarantined"
			old := &queuedExchange{Exchange: Exchange{ID: "old-stop", Conversation: "console:main", Input: "/cancel", Key: "client:old", State: "failed", EnqueuedAt: time.Now()}, Receipt: &consoleapi.Reply{Error: target.RecoveryStopPending}}
			s.exchanges["console:main"] = append(s.exchanges["console:main"], old)
			s.questions["old-question"] = consoleapi.PendingQuestion{Conversation: "console:main", ExchangeID: target.ID, AttemptID: "original-attempt", TaskID: "task-1", Principal: "owner"}
			switch mode {
			case "missing-attempt":
				old.Receipt.Error = "unconfirmed"
				target.RecoveryStopPending = "unconfirmed"
			case "different-error":
				old.Receipt.Error = "attempt other-attempt writer is quarantined"
			case "newer-task":
				target.EnqueuedAt = old.EnqueuedAt.Add(time.Second)
			}
			got, err := s.retryRecoveryStopLocked(old)
			if err != nil || got != old || got.RecoveryStopTarget != nil || got.State != "failed" {
				t.Fatalf("old stop selected unrelated recovery: %+v %v", got, err)
			}
			s.mu.Unlock()
		})
	}
}

// cancelledTaskDriver holds the original execution of a task a person
// may set aside: nothing resumes it any more, and its stop is confirmed
// once confirmed says so. exchange is the exchange the execution answers.
// states is what the task store holds for each task, running unless set;
// like the task store, stopping a task that is not paused cancels it.
type cancelledTaskDriver struct {
	*recoveryDriver
	exchange  string
	explicit  atomic.Bool
	stops     atomic.Int32
	confirmed atomic.Bool

	mu     sync.Mutex
	states map[string]task.State
}

func newCancelledTaskDriver() *cancelledTaskDriver {
	return &cancelledTaskDriver{exchange: "e1", recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "The task's authorization changed.", Choices: []view.Choice{{Value: "retry", Label: "Retry"}}}, Cause: errors.New("task execution was stopped")}
	}}}
}

// RetainedChatsFor finds the execution with its task's durable state.
func (d *cancelledTaskDriver) RetainedChatsFor(ctx context.Context, conversation, messageID string) ([]turn.RetainedChat, error) {
	items, err := d.recoveryDriver.RetainedChatsFor(ctx, conversation, messageID)
	out := make([]turn.RetainedChat, 0, len(items))
	for _, item := range items {
		item.TaskState = d.state(item.TaskID)
		out = append(out, item)
	}
	return out, err
}

func (d *cancelledTaskDriver) state(id string) task.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	if state, ok := d.states[id]; ok {
		return state
	}
	return task.StateRunning
}

// setAside records ids as moved to state in the task store, without the
// console hearing of it.
func (d *cancelledTaskDriver) setAside(state task.State, ids ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.states == nil {
		d.states = map[string]task.State{}
	}
	for _, id := range ids {
		d.states[id] = state
	}
}

// cancelTasks cancels ids the way the task list does: the task store
// records it, then the console is told.
func cancelTasks(s *Service, d *cancelledTaskDriver, ids ...string) {
	d.setAside(task.StateCancelled, ids...)
	s.TasksSetAside()
}

func (d *cancelledTaskDriver) StopRetainedTask(_ context.Context, id string, req turn.Request, cancel bool) (turn.Result, error) {
	if cancel {
		d.explicit.Store(true)
	}
	d.stops.Add(1)
	if id != "task-1" || req.ConversationID != "console:main" || req.MessageID != AnchorMark+d.exchange || req.SenderOpenID != "owner" {
		return turn.Result{}, errors.New("stop targeted another execution")
	}
	if cancel || d.state(id) != task.StatePaused {
		d.setAside(task.StateCancelled, id)
	}
	if !d.confirmed.Load() {
		return turn.Result{}, harness.ErrStopUnconfirmed
	}
	return turn.Result{Text: "task #1 cancelled"}, nil
}

// awaitOffer waits for the pending question that offers choice.
func awaitOffer(t *testing.T, s *Service, choice string) consoleapi.PendingQuestion {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		for _, q := range s.Questions("main") {
			for _, option := range q.Options {
				if q.State == "pending" && option.ID == choice {
					return q
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no pending question offers %q: %+v", choice, s.Questions("main"))
	return consoleapi.PendingQuestion{}
}

func questionByID(s *Service, id string) consoleapi.PendingQuestion {
	for _, q := range s.Questions("main") {
		if q.ID == id {
			return q
		}
	}
	return consoleapi.PendingQuestion{}
}

// A task cancelled while its exchange runs or recovers can no longer be
// resumed, so the recovery stops offering to and turns to confirming that
// the original execution stopped: the exchange closes as cancelled once
// the stop is confirmed, and until then it waits on a card that says the
// stop is being confirmed. These consoles keep no task store to read, so
// the queue behind that card stays held; TestStopWaitExchangeDoesNotHoldQueue
// covers the line moving past it.
func TestCancelledTaskTurnsItsRecoveryIntoConfirmingTheStop(t *testing.T) {
	t.Run("stop confirms", func(t *testing.T) {
		s := impatient(New(&echo{}, "owner", nil))
		s.EnableRetainedRecovery(t.Context())
		if err := s.Persist(recoveryDocument()); err != nil {
			t.Fatal(err)
		}
		driver := newCancelledTaskDriver()
		driver.confirmed.Store(true)
		if err := s.RecoverChats(t.Context(), driver); err != nil {
			t.Fatal(err)
		}
		asked := awaitOffer(t, s, "retry")
		// Another task's cancellation leaves this question to the owner.
		cancelTasks(s, driver, "task-9")
		time.Sleep(30 * time.Millisecond)
		if q := questionByID(s, asked.ID); q.State != "pending" || driver.stops.Load() != 0 {
			t.Fatalf("another task's cancellation settled this recovery: question %+v, stops %d", q, driver.stops.Load())
		}
		cancelTasks(s, driver, "task-1", "task-2")
		if got := awaitExchange(t, s, "e1"); got.State != consoleapi.ExchangeCancelled {
			t.Fatalf("exchange of a cancelled task whose stop confirmed = %+v, want cancelled", got)
		}
		if q := questionByID(s, asked.ID); q.State != "cancelled" || q.Answer != nil {
			t.Fatalf("the offer to retry a cancelled task was not withdrawn: %+v", q)
		}
		awaitExchange(t, s, "e2")
	})
	t.Run("stop unconfirmed", func(t *testing.T) {
		doc := recoveryDocument()
		s := impatient(New(&echo{}, "owner", nil))
		s.recoveryStopEvery = 20 * time.Millisecond
		s.EnableRetainedRecovery(t.Context())
		if err := s.Persist(doc); err != nil {
			t.Fatal(err)
		}
		driver := newCancelledTaskDriver()
		if err := s.RecoverChats(t.Context(), driver); err != nil {
			t.Fatal(err)
		}
		asked := awaitOffer(t, s, "retry")
		cancelTasks(s, driver, "task-1")
		awaitOffer(t, s, "recheck")
		if q := questionByID(s, asked.ID); q.State != "cancelled" {
			t.Fatalf("the offer to retry a cancelled task is still open: %+v", q)
		}
		if got := s.Queue("main"); got[0].State != consoleapi.ExchangeAwaitingUser || got[1].State != consoleapi.ExchangeQueued {
			t.Fatalf("an unconfirmed stop released the queue: %+v", got)
		}
		// The stop is what the exchange waits on now, across a restart too.
		if raw, _, _ := doc.Load(); !strings.Contains(string(raw), `"recovery_stop_pending"`) {
			t.Fatalf("the stop of the cancelled task was not recorded: %s", raw)
		}
		driver.confirmed.Store(true)
		if got := awaitExchange(t, s, "e1"); got.State != consoleapi.ExchangeCancelled {
			t.Fatalf("a confirmed recheck did not settle the exchange: %+v", got)
		}
		awaitExchange(t, s, "e2")
	})
	t.Run("owner declined to retry", func(t *testing.T) {
		s := impatient(New(&echo{}, "owner", nil))
		s.EnableRetainedRecovery(t.Context())
		if err := s.Persist(recoveryDocument()); err != nil {
			t.Fatal(err)
		}
		driver := newCancelledTaskDriver()
		if err := s.RecoverChats(t.Context(), driver); err != nil {
			t.Fatal(err)
		}
		asked := awaitOffer(t, s, "retry")
		if _, err := s.AnswerQuestion(t.Context(), asked.ID, consoleapi.QuestionAnswer{CommandID: "leave-it", Decision: "decline"}); err != nil {
			t.Fatal(err)
		}
		// The recovery now waits quietly between passes instead of asking.
		for deadline := time.Now().Add(2 * time.Second); driver.calls.Load() < 2; {
			if time.Now().After(deadline) {
				t.Fatal("recovery did not look again after the owner declined")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(10 * time.Millisecond)
		cancelTasks(s, driver, "task-1")
		awaitOffer(t, s, "recheck")
		if driver.stops.Load() == 0 {
			t.Fatal("the stop of the cancelled task was not attempted")
		}
	})
	t.Run("cancelled while still running", func(t *testing.T) {
		h := &queueHandler{started: make(chan *queueCall, 1)}
		s := impatient(New(h, "owner", nil))
		s.EnableRetainedRecovery(t.Context())
		if err := s.Persist(&memDoc{}); err != nil {
			t.Fatal(err)
		}
		driver := newCancelledTaskDriver()
		driver.confirmed.Store(true)
		if err := s.RecoverChats(t.Context(), driver); err != nil {
			t.Fatal(err)
		}
		e := enqueueForTest(t, s, "main", "original prompt")
		call := nextCall(t, h)
		driver.exchange = e.ID
		driver.candidates = []turn.RetainedChat{{AttemptID: "attempt-1", TaskID: "task-1", Conversation: e.Conversation, MessageID: AnchorMark + e.ID}}
		// The task is cancelled while the turn still runs, and only then
		// does the turn leave its execution to recovery.
		cancelTasks(s, driver, "task-1")
		call.finish <- harness.ErrStopUnconfirmed
		for deadline := time.Now().Add(2 * time.Second); driver.stops.Load() == 0; time.Sleep(time.Millisecond) {
			for _, q := range s.Questions("main") {
				for _, option := range q.Options {
					if option.ID == "retry" {
						t.Fatalf("the recovery of a task cancelled while it ran offered to retry it: state=%s options=%+v", q.State, q.Options)
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the stop of a task cancelled while it ran was not attempted: %+v", s.Queue("main"))
			}
		}
		if got := awaitExchange(t, s, e.ID); got.State != consoleapi.ExchangeCancelled {
			t.Fatalf("exchange of a task cancelled while it ran = %+v, want cancelled", got)
		}
		if driver.calls.Load() != 0 {
			t.Fatalf("a task cancelled while it ran was resumed %d times", driver.calls.Load())
		}
	})
}

// unreachableStopDriver finds the original execution of a cancelled task
// only as many more times as left allows once limited is set; every
// lookup after that fails.
type unreachableStopDriver struct {
	*cancelledTaskDriver
	limited atomic.Bool
	left    atomic.Int32
}

func (d *unreachableStopDriver) RetainedChatsFor(ctx context.Context, conversation, messageID string) ([]turn.RetainedChat, error) {
	if d.limited.Load() && d.left.Add(-1) < 0 {
		return nil, errors.New("retained lookup unavailable")
	}
	return d.cancelledTaskDriver.RetainedChatsFor(ctx, conversation, messageID)
}

// The stop of a cancelled task is recorded before it is tried, so even a
// stop that cannot find the original execution to stop leaves a restart
// waiting on the stop instead of offering to resume the task again.
func TestCancelledTaskStopIsRecordedBeforeItIsTried(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	doc := recoveryDocument()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(lifetime)
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	driver := &unreachableStopDriver{cancelledTaskDriver: newCancelledTaskDriver()}
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	awaitOffer(t, s, "retry")
	// The watch that reads the cancellation off the task store and the pass
	// after it still find the original; the stop that follows does not.
	driver.left.Store(2)
	driver.limited.Store(true)
	cancelTasks(s, driver.cancelledTaskDriver, "task-1")
	awaitOffer(t, s, "recheck")
	if raw, _, _ := doc.Load(); !strings.Contains(string(raw), `"recovery_stop_pending"`) {
		t.Fatalf("the stop of the cancelled task was not recorded: %s", raw)
	}
	if driver.stops.Load() != 0 {
		t.Fatalf("a stop that could not find the original reached it %d times", driver.stops.Load())
	}
	cancel()
	s.workers.Wait()
	restored := impatient(New(&echo{}, "owner", nil))
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	restart := newCancelledTaskDriver()
	restart.setAside(task.StateCancelled, "task-1")
	if err := restored.RecoverChats(t.Context(), restart); err != nil {
		t.Fatal(err)
	}
	awaitOffer(t, restored, "recheck")
	for _, q := range restored.Questions("main") {
		for _, option := range q.Options {
			if q.State == "pending" && option.ID == "retry" {
				t.Fatalf("a restart offered to retry a cancelled task: %+v", q)
			}
		}
	}
	if got := restored.Queue("main"); got[0].State != consoleapi.ExchangeAwaitingUser || got[1].State != consoleapi.ExchangeQueued || restart.calls.Load() != 0 {
		t.Fatalf("a restart did not wait on the stop of a cancelled task: queue %+v, resumes %d", got, restart.calls.Load())
	}
}

// A question the watch withdraws before it even reaches the owner was
// never answered either, so the recovery treats it as withdrawn instead
// of detaching as though its own lifetime had ended.
func TestRecoveryQuestionWithdrawnBeforeItIsPut(t *testing.T) {
	for _, why := range []string{"task cancelled", "original back"} {
		t.Run(why, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			s := impatient(New(&echo{}, "owner", nil))
			e := &queuedExchange{Exchange: Exchange{ID: "e1", Conversation: "console:main", State: consoleapi.ExchangeAwaitingUser}}
			var driver RetainedChatDriver
			cancels := make(chan struct{}, 1)
			switch why {
			case "task cancelled":
				cancelled := newCancelledTaskDriver()
				cancelled.setAside(task.StateCancelled, "task-1")
				driver = cancelled
				cancels <- struct{}{}
			case "original back":
				back := &probingDriver{}
				back.reachable.Store(true)
				driver = back
			}
			r := &exchangeRecovery{s: s, ctx: ctx, e: e, exchange: e.Exchange, driver: driver, cancels: cancels}
			identity := &questionIdentity{base: consoleapi.PendingQuestion{Conversation: "console:main", ExchangeID: "e1", TaskID: "task-1", AttemptID: "attempt-1"}}
			// The watch settles the question before it is put to the owner.
			put := func(ctx context.Context, binding consoleapi.PendingQuestion, question view.Question) (view.Answer, error) {
				<-ctx.Done()
				return s.turnQuestion(ctx, binding, question)
			}
			question := view.Question{Kind: "recovery", Message: "offline", Choices: []view.Choice{{Value: "retry", Label: "Retry"}}}
			answer, err := r.ask(identity, question, put)
			if err != nil || answer != (view.Answer{}) || !r.withdrawn {
				t.Fatalf("question withdrawn before it was put: answer %+v, err %v, withdrawn %v", answer, err, r.withdrawn)
			}
			if got := s.Questions("main"); len(got) != 0 {
				t.Fatalf("a withdrawn question reached the owner: %+v", got)
			}
		})
	}
}
