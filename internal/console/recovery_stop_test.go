package console

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type stoppingRecoveryDriver struct {
	*recoveryDriver
	stops    atomic.Int32
	stopErr  error
	stopHook func()
}

func (d *stoppingRecoveryDriver) StopRetainedTask(_ context.Context, id string, req turn.Request) (turn.Result, error) {
	d.stops.Add(1)
	if d.stopHook != nil {
		d.stopHook()
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
			s := New(&echo{}, "owner", nil)
			s.EnableRetainedRecovery(lifetime)
			if err := s.Persist(doc); err != nil {
				t.Fatal(err)
			}
			driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
				return turn.Result{}, &turn.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "Open was not acknowledged. Check the original machine.", Choices: []view.Choice{{Value: "retry", Label: "Retry"}}}}
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
				if got := awaitExchange(t, s, "e1"); !terminalExchange(got.State) {
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
			if driver.stops.Load() != 1 {
				t.Fatal("same stop command repeated side effects")
			}
			cancel()
			s.workers.Wait()
			restored := New(&echo{}, "owner", nil)
			restored.EnableRetainedRecovery(t.Context())
			if err := restored.Persist(doc); err != nil {
				t.Fatal(err)
			}
			got := restored.Queue("main")[0]
			if uncertain && terminalExchange(got.State) {
				t.Fatal("restart lost uncertainty")
			}
			if !uncertain && !terminalExchange(got.State) {
				t.Fatal("restart resurrected stopped task")
			}
		})
	}
}

func TestRestartAfterConfirmedRecoveryStopDoesNotContactExecutorAgain(t *testing.T) {
	doc := recoveryDocument()
	s := New(&echo{}, "owner", nil)
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
	restored := New(&echo{}, "owner", nil)
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
	s := New(&echo{}, "owner", nil)
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
	s := New(&echo{}, "owner", nil)
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
	s := New(&echo{}, "owner", nil)
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
	s := New(&echo{}, "owner", nil)
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
	restored := New(&echo{}, "owner", nil)
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

func TestDetachedUnconfirmedRecoveryDoesNotReleaseQueuedInstructions(t *testing.T) {
	s := New(&echo{}, "owner", nil)
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
	if got := s.Queue("main"); terminalExchange(got[0].State) || got[1].State != "queued" {
		t.Fatalf("detached uncertainty released queue: %+v", got)
	}
}

func TestRecoveryStopIntentIsDurableBeforeStoppingExecutions(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	doc := recoveryDocument()
	s := New(&echo{}, "owner", nil)
	s.EnableRetainedRecovery(lifetime)
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	var snapshot *memDoc
	driver := &stoppingRecoveryDriver{recoveryDriver: &recoveryDriver{resume: func(context.Context, string, turn.Request) (turn.Result, error) {
		return turn.Result{}, &turn.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "open not acknowledged", Choices: []view.Choice{{Value: "retry", Label: "Retry"}}}}
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
	restored := New(&echo{}, "owner", nil)
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
	waitRecoveryQuestion(t, restored)
	if got := restored.Queue("main"); got[0].State != "awaiting-user" || got[1].State != "queued" {
		t.Fatalf("in-flight stop lost after crash: %+v", got)
	}
	if _, err := restored.SendCommand(t.Context(), "main", "/cancel", "retry-stop-after-crash"); err != nil {
		t.Fatalf("interrupted stop command became an unrelated recovery task: %v", err)
	}
	if got := awaitExchange(t, restored, "e1"); got.State != "cancelled" {
		t.Fatalf("stop could not complete after restart: %+v", got)
	}
	awaitExchange(t, restored, "e2")
}

func TestNewInputBehindDetachedRecoveryIsDurable(t *testing.T) {
	doc := recoveryDocument()
	s := New(&echo{}, "owner", nil)
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
	restored := New(&echo{}, "owner", nil)
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
	restored := New(&echo{}, "owner", nil)
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	got := restored.Queue("main")
	if got[0].State != "recovering" || got[1].State != "failed" {
		t.Fatalf("stop became a native recovery: %+v", got)
	}
}
