package console

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
)

type initializationDriver struct {
	recoveryDriver
	chats, plans atomic.Int32
	failChats    atomic.Bool
	failPlans    atomic.Bool
}

func (d *initializationDriver) RetainedChats(ctx context.Context) ([]turn.RetainedChat, error) {
	d.chats.Add(1)
	if d.failChats.Load() {
		return nil, errors.New("chat initialization unavailable")
	}
	return d.recoveryDriver.RetainedChats(ctx)
}

func (d *initializationDriver) RetainedPlans(context.Context) ([]turn.RetainedPlan, error) {
	d.plans.Add(1)
	if d.failPlans.Load() {
		return nil, errors.New("plan initialization unavailable")
	}
	return nil, nil
}

func (*initializationDriver) ResumeRetainedPlan(context.Context, turn.RetainedPlan, turn.Request) (turn.Result, error) {
	panic("no retained plan")
}

func initializedRecoveryConsole(t *testing.T) (*Service, *ledger.Ledger) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := New(&echo{}, "owner", nil)
	lifetime, cancel := context.WithCancel(t.Context())
	s.EnableRetainedRecovery(lifetime)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		book.Close()
	})
	return s, book
}

func TestRecoverChatsInitializedDriverSkipsIdleHistoryPreflight(t *testing.T) {
	s, _ := initializedRecoveryConsole(t)
	driver := &initializationDriver{}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	// A later history failure must not disable the already initialized
	// generation's idle reconciliation.
	driver.failChats.Store(true)
	driver.failPlans.Store(true)
	for range 3 {
		if err := s.RecoverChats(t.Context(), driver); err != nil {
			t.Fatalf("initialized driver repeated history preflight: %v", err)
		}
	}
	if driver.chats.Load() != 1 || driver.plans.Load() != 1 {
		t.Fatalf("idle reconciliation read history: chats=%d plans=%d", driver.chats.Load(), driver.plans.Load())
	}
	replacement := &initializationDriver{}
	if err := s.RecoverChats(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.chats.Load() != 1 || replacement.plans.Load() != 1 {
		t.Fatal("new driver skipped initialization")
	}
}

func TestRecoverChatsFailedInitializationRemainsRetryable(t *testing.T) {
	for _, failure := range []string{"chats", "plans"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := initializedRecoveryConsole(t)
			driver := &initializationDriver{}
			if failure == "chats" {
				driver.failChats.Store(true)
			} else {
				driver.failPlans.Store(true)
			}
			if err := s.RecoverChats(t.Context(), driver); err == nil {
				t.Fatal("initialization failure hidden")
			}
			s.mu.Lock()
			installed := s.recoveryDriver
			s.mu.Unlock()
			if installed != nil {
				t.Fatal("failed initialization installed driver")
			}
			driver.failChats.Store(false)
			driver.failPlans.Store(false)
			if err := s.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			chats, plans := driver.chats.Load(), driver.plans.Load()
			if chats != 2 || plans == 0 {
				t.Fatal("failed initialization was not retried")
			}
			if err := s.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			if driver.chats.Load() != chats || driver.plans.Load() != plans {
				t.Fatal("successful retry was not remembered")
			}
		})
	}
}

func TestRecoverChatsInitializedDriverRestartsDetachedWorker(t *testing.T) {
	s, book := initializedRecoveryConsole(t)
	e := &queuedExchange{Exchange: Exchange{ID: "e1", Conversation: "console:main", Input: "original", State: consoleapi.ExchangeRecovering}, done: make(chan struct{})}
	s.mu.Lock()
	s.exchanges[e.Conversation] = []*queuedExchange{e}
	s.running[e.Conversation] = 1
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	driver := &initializationDriver{recoveryDriver: recoveryDriver{resume: func(ctx context.Context, id string, _ turn.Request) (turn.Result, error) {
		select {
		case started <- struct{}{}:
			<-ctx.Done()
			return turn.Result{}, ctx.Err()
		default:
			return turn.Result{Text: "original completed result", Attempt: id}, nil
		}
	}}}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); len(started) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(time.Millisecond)
	}
	// Detach this worker, not the service generation. The persisted exchange
	// remains recovering and the next runtime pass must rejoin it.
	s.mu.Lock()
	e.cancel()
	s.mu.Unlock()
	s.workers.Wait()
	s.mu.Lock()
	detached := e.cancel == nil && e.State == consoleapi.ExchangeRecovering
	s.mu.Unlock()
	if !detached {
		t.Fatal("fixture did not detach the pending worker")
	}
	chats, plans := driver.chats.Load(), driver.plans.Load()
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	s.workers.Wait()
	if got := awaitExchange(t, s, "e1"); got.State != consoleapi.ExchangeDone {
		t.Fatalf("detached worker was not recovered: %+v", got)
	}
	// Only the restarted pending worker reads retained evidence; there is no
	// extra full-history preflight from RecoverChats itself.
	if driver.chats.Load() != chats+1 || driver.plans.Load() != plans+1 || driver.calls.Load() != 2 {
		t.Fatalf("wrong recovery reads/calls: chats=%d plans=%d resumes=%d", driver.chats.Load()-chats, driver.plans.Load()-plans, driver.calls.Load())
	}
	s.mu.Lock()
	running := s.running[e.Conversation]
	s.mu.Unlock()
	if running != 0 {
		t.Fatalf("detached worker left a conversation reservation: %d", running)
	}
	restored := New(&echo{}, "owner", nil)
	if err := restored.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	if got := restored.Queue("main"); len(got) != 1 || got[0].State != consoleapi.ExchangeDone {
		t.Fatalf("recovered result was not durable: %+v", got)
	}
}
