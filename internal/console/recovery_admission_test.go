package console

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"github.com/gopact-ai/steve/internal/view"
)

// This port represents the execution owner's durable proof, not the console's
// retained list. An empty retained list alone is deliberately inconclusive.
type admissionRecoveryDriver struct {
	*recoveryDriver
	proven    atomic.Bool
	err       error
	checks    atomic.Int32
	check     func(turn.Request)
	lookupErr atomic.Bool
}

func (d *admissionRecoveryDriver) RetainedChatsFor(ctx context.Context, conversation, messageID string) ([]turn.RetainedChat, error) {
	if d.lookupErr.Load() {
		return nil, errors.New("retained identity read failed")
	}
	return d.recoveryDriver.RetainedChatsFor(ctx, conversation, messageID)
}

func (d *admissionRecoveryDriver) ConfirmNeverAdmitted(_ context.Context, req turn.Request) (bool, error) {
	d.checks.Add(1)
	if d.check != nil {
		d.check(req)
	}
	return d.proven.Load(), d.err
}

func newAdmissionRecoveryDriver() *admissionRecoveryDriver {
	return &admissionRecoveryDriver{recoveryDriver: &recoveryDriver{candidates: []turn.RetainedChat{}}}
}

func admissionRecoveryFixture(t *testing.T) (*Service, *ledger.Ledger, *queueHandler) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	state := DurableState{Exchanges: map[string][]DurableExchange{"console:main": {
		{Exchange: Exchange{ID: "e1", Conversation: "console:main", Input: "original", Key: "client:original", State: consoleapi.ExchangeRunning, ExpectedProject: "project", ExpectedTask: "task"}},
		{Exchange: Exchange{ID: "e2", Conversation: "console:main", Input: "follow-up", State: consoleapi.ExchangeQueued}},
	}}}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return StoreStateTx(tx, state) }); err != nil {
		t.Fatal(err)
	}
	h := &queueHandler{started: make(chan *queueCall, 2)}
	s := impatient(New(h, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s, book, h
}

func checkAdmissionRequest(t *testing.T, req turn.Request) {
	t.Helper()
	if req.Channel != "console" || req.ConversationID != "console:main" || req.MessageID != "web-e1" ||
		req.ExchangeID != "e1" || req.SenderOpenID != "owner" || req.ExpectedProject != "project" || req.ExpectedTask != "task" {
		t.Errorf("proof requested for wrong identity: %+v", req)
	}
}

func TestRecoveryNeverAdmittedFinishesWithoutReplayAndSurvivesRestart(t *testing.T) {
	s, book, h := admissionRecoveryFixture(t)
	driver := newAdmissionRecoveryDriver()
	driver.proven.Store(true)
	driver.check = func(req turn.Request) { checkAdmissionRequest(t, req) }
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	if got := awaitExchange(t, s, "e1"); got.State != consoleapi.ExchangeFailed || got.ReplyID == "" {
		t.Fatalf("never-admitted exchange did not finish: %+v", got)
	}
	next := nextCall(t, h)
	if next.req.Input != "follow-up" {
		t.Fatalf("replayed original prompt: %+v", next.req)
	}
	next.finish <- nil
	awaitExchange(t, s, "e2")
	if driver.calls.Load() != 0 || driver.checks.Load() == 0 {
		t.Fatal("recovery resumed an execution instead of consuming admission proof")
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored := New(h, "owner", nil)
	restored.EnableRetainedRecovery(t.Context())
	if err := restored.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	if got := restored.Queue("main")[0]; got.State != consoleapi.ExchangeFailed || got.ReplyID == "" {
		t.Fatalf("restart lost never-admitted receipt: %+v", got)
	}
	if err := restored.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	if err := restored.Drain(); err != nil {
		t.Fatal(err)
	}
	noCall(t, h)
}

func TestRecoveryNeverAdmittedCanCancelThroughControlOrQuestion(t *testing.T) {
	for _, question := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "question"}[question], func(t *testing.T) {
			s, book, h := admissionRecoveryFixture(t)
			driver := newAdmissionRecoveryDriver()
			driver.check = func(req turn.Request) { checkAdmissionRequest(t, req) }
			if err := s.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			waitRecoveryQuestion(t, s)
			driver.proven.Store(true)
			if question {
				s.mu.Lock()
				target := s.exchanges["console:main"][0]
				s.mu.Unlock()
				if err := s.cancelRecovering(t.Context(), target, "owner"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-original"); err != nil {
				t.Fatal(err)
			}
			if got := awaitExchange(t, s, "e1"); got.State != consoleapi.ExchangeCancelled {
				t.Fatalf("proven unadmitted exchange not cancelled: %+v", got)
			}
			next := nextCall(t, h)
			if next.req.Input != "follow-up" {
				t.Fatalf("cancel replayed original: %+v", next.req)
			}
			next.finish <- nil
			awaitExchange(t, s, "e2")
			state, err := LoadState(book)
			if err != nil {
				t.Fatal(err)
			}
			if receipt := state.Exchanges["console:main"][0].RecoveryStop; receipt == nil || receipt.Error != "" {
				t.Fatalf("missing durable cancellation receipt: %+v", receipt)
			}
			if driver.calls.Load() != 0 {
				t.Fatal("stop attempted to resume nonexistent execution")
			}
		})
	}
}

func TestRecoveryMissingAdmissionProofKeepsLostExecutionsProtected(t *testing.T) {
	for _, proofErr := range []error{nil, errors.New("accounting unavailable")} {
		t.Run(map[bool]string{false: "inconclusive", true: "proof-error"}[proofErr != nil], func(t *testing.T) {
			s, book, h := admissionRecoveryFixture(t)
			driver := newAdmissionRecoveryDriver()
			driver.err = proofErr
			// Even true cannot authorize cancellation when its read failed.
			driver.proven.Store(proofErr != nil)
			if err := s.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			waitRecoveryQuestion(t, s)
			if _, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-original"); !errors.Is(err, harness.ErrStopUnconfirmed) {
				t.Fatalf("missing execution proof must keep stop unconfirmed: %v", err)
			}
			if got := s.Queue("main"); got[0].State.Terminal() || got[1].State != consoleapi.ExchangeQueued {
				t.Fatalf("uncertainty released original or FIFO: %+v", got)
			}
			noCall(t, h)
			if err := s.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored := New(h, "owner", nil)
			restored.EnableRetainedRecovery(t.Context())
			if err := restored.PersistLedger(book); err != nil {
				t.Fatal(err)
			}
			if got := restored.Queue("main"); got[0].State.Terminal() || got[1].State != consoleapi.ExchangeQueued {
				t.Fatalf("restart forgot uncertain execution: %+v", got)
			}
		})
	}
}

func TestRecoveryAdmissionProofCannotOverrideRetainedExecutionOrLookupError(t *testing.T) {
	for _, failedLookup := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "lookup-error"}[failedLookup], func(t *testing.T) {
			s, _, h := admissionRecoveryFixture(t)
			driver := newAdmissionRecoveryDriver()
			driver.proven.Store(true)
			driver.candidates = []turn.RetainedChat{{Conversation: "console:main", MessageID: "web-e1", TaskID: "task", AttemptID: "live"}}
			driver.resume = func(context.Context, string, turn.Request) (turn.Result, error) {
				return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Message: "original execution is unreachable"}}
			}
			if err := s.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			waitRecoveryQuestion(t, s)
			driver.lookupErr.Store(failedLookup)
			if _, err := s.SendCommand(t.Context(), "main", "/cancel", "stop-original"); err == nil {
				t.Fatal("uncertain execution was cancelled without physical stop evidence")
			}
			if driver.checks.Load() != 0 {
				t.Fatal("consulted no-admission proof over retained execution/read failure")
			}
			if got := s.Queue("main"); got[0].State.Terminal() || got[1].State != consoleapi.ExchangeQueued {
				t.Fatalf("protected execution or FIFO released: %+v", got)
			}
			noCall(t, h)
		})
	}
}

type preparationFailureHandler struct {
	turntest.IdleCoordinator
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (h *preparationFailureHandler) Handle(context.Context, turn.Request) (turn.Result, error) {
	h.calls.Add(1)
	close(h.started)
	<-h.release
	return turn.Result{}, errors.New("prepareSession: revoked session")
}

func TestRecoveryPreAdmissionErrorFinishesOrRecoversAfterGenerationShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "generation-shutdown"}[shutdown], func(t *testing.T) {
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := &preparationFailureHandler{started: make(chan struct{}), release: make(chan struct{})}
			s := New(h, "owner", nil)
			s.EnableRetainedRecovery(lifetime)
			doc := &memDoc{}
			if err := s.Persist(doc); err != nil {
				t.Fatal(err)
			}
			e := enqueueForTest(t, s, "main", "original")
			<-h.started
			if shutdown {
				cancel()
			}
			close(h.release)
			s.workers.Wait()
			if !shutdown {
				if got := awaitExchange(t, s, e.ID); got.State != consoleapi.ExchangeFailed {
					t.Fatalf("healthy preparation error did not finish: %+v", got)
				}
				return
			}
			if err := s.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored := New(h, "owner", nil)
			restored.EnableRetainedRecovery(t.Context())
			if err := restored.Persist(doc); err != nil {
				t.Fatal(err)
			}
			driver := newAdmissionRecoveryDriver()
			driver.proven.Store(true)
			if err := restored.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			if got := awaitExchange(t, restored, e.ID); got.State != consoleapi.ExchangeFailed {
				t.Fatalf("interrupted preparation error remained stuck: %+v", got)
			}
			if h.calls.Load() != 1 || driver.calls.Load() != 0 {
				t.Fatal("generation recovery replayed original input")
			}
		})
	}
}
