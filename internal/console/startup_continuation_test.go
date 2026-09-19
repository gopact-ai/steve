package console

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestStartupContinuationPersistsBeforeDrainAndDeduplicatesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := &queueHandler{started: make(chan *queueCall, 3)}
	s := New(h, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	queue := func(s *Service, taskID, prompt string) error {
		return s.QueueTaskResume(t.Context(), "main", taskID, "resume:original", "worker", "continue original task", prompt)
	}
	if err := queue(s, "parent", "original prompt"); err != nil {
		t.Fatal(err)
	}
	if err := queue(s, "other", "wrong prompt"); err == nil {
		t.Fatal("accepted same key for another task")
	}
	noCall(t, h)
	id := s.Queue("main")[0].ID
	if got := s.Queue("main"); len(got) != 1 || got[0].State != consoleapi.ExchangeQueued || got[0].ExpectedTask != "parent" {
		t.Fatalf("startup was not deferred: %+v", got)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	restored := New(h, "owner", nil)
	if err := restored.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	if err := queue(restored, "parent", "must not replace frozen input"); err != nil {
		t.Fatal(err)
	}
	if len(restored.Queue("main")) != 1 || restored.Queue("main")[0].ID != id {
		t.Fatal("restart duplicated the continuation")
	}
	noCall(t, h)
	if err := restored.Drain(); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	if call.req.ExpectedTask != "parent" || call.req.Input != "@worker original prompt" {
		t.Fatalf("lost original binding/input: %+v", call.req)
	}
	call.finish <- nil
	awaitExchange(t, restored, id)
	if err := queue(restored, "parent", "already done"); err != nil {
		t.Fatal(err)
	}
	if err := restored.Drain(); err != nil {
		t.Fatal(err)
	}
	noCall(t, h)
	// Ordinary ingress must retain its eager-start behavior.
	e, err := restored.Enqueue(t.Context(), "other", "ordinary input", nil)
	if err != nil {
		t.Fatal(err)
	}
	nextCall(t, h).finish <- nil
	awaitExchange(t, restored, e.ID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := restored.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStartupContinuationPersistenceFailureCanRetry(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	h := &queueHandler{started: make(chan *queueCall, 1)}
	s := New(h, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	_, err = book.DB().Exec(`CREATE TRIGGER reject_startup_queue BEFORE INSERT ON bindings WHEN NEW.kind LIKE 'console-%' BEGIN SELECT RAISE(ABORT,'startup queue unavailable'); END`)
	if err != nil {
		t.Fatal(err)
	}
	queue := func() error {
		return s.QueueTaskResume(t.Context(), "main", "parent", "resume:original", "worker", "notice", "prompt")
	}
	if err := queue(); err == nil || !strings.Contains(err.Error(), "startup queue unavailable") {
		t.Fatalf("lost save failure: %v", err)
	}
	if len(s.Queue("main")) != 0 {
		t.Fatal("failed save retained memory acceptance")
	}
	noCall(t, h)
	if _, err := book.DB().Exec(`DROP TRIGGER reject_startup_queue`); err != nil {
		t.Fatal(err)
	}
	if err := queue(); err != nil {
		t.Fatal(err)
	}
	noCall(t, h)
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	nextCall(t, h).finish <- nil
	awaitExchange(t, s, s.Queue("main")[0].ID)
}

func TestDeferredRecoveryCannotBeStartedByOrdinaryQueueCompletion(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	h := &queueHandler{started: make(chan *queueCall, 3)}
	s := New(h, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	active, err := s.Enqueue(t.Context(), "main", "active", nil)
	if err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	if err := s.QueueTaskResume(t.Context(), "main", "parent", "resume:old", "worker", "notice", "prompt"); err != nil {
		t.Fatal(err)
	}
	call.finish <- nil
	awaitExchange(t, s, active.ID)
	noCall(t, h)
	if len(s.Queue("main")) != 2 || s.Queue("main")[1].State != consoleapi.ExchangeQueued {
		t.Fatal("completion bypassed recovery barrier")
	}
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	nextCall(t, h).finish <- nil
	awaitExchange(t, s, s.Queue("main")[1].ID)
}

func TestStartupPendingSteerPreservesInputAndAllowsStopControl(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	h := &queueHandler{started: make(chan *queueCall, 3)}
	s := New(h, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueTaskResume(t.Context(), "main", "parent", "startup", "worker", "notice", "prompt"); err != nil {
		t.Fatal(err)
	}
	before := s.Queue("main")[0]
	if _, err := s.Steer(t.Context(), before.ID); err == nil {
		t.Fatal("Steer bypassed original accounting")
	}
	after := s.Queue("main")[0]
	if before.Input != after.Input || after.State != consoleapi.ExchangeQueued || !after.StartedAt.IsZero() {
		t.Fatalf("failed dispatch mutated accepted input: before=%+v after=%+v", before, after)
	}
	noCall(t, h)
	control, err := s.Enqueue(t.Context(), "main", "/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	nextCall(t, h).finish <- nil
	awaitExchange(t, s, control.ID)
	noCall(t, h)
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	nextCall(t, h).finish <- nil
	awaitExchange(t, s, before.ID)
}
