package project

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func cloneFixture(t *testing.T) (*Store, Project, CloneOperation, func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	book, err := ledger.Open(t.TempDir(), ledger.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	s := Open(book)
	s.now = func() time.Time { return now }
	c := Copy{Node: "remote", Path: "/clone", Origin: OriginCloned, Source: "source", State: CopyProvisioning}
	p := Project{ID: "p", Home: Home{Path: "/p"}, Copies: map[string]Copy{"remote": c}}
	if err := s.Reconcile(t.Context(), []Project{p}, "one"); err != nil {
		t.Fatal(err)
	}
	op, err := s.BeginClone(t.Context(), p.ID, c, "", "test", 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return s, p, op, func(delta time.Duration) { now = now.Add(delta) }
}

func TestCloneIsolationOutlivesLeaseAndRequiresStopEvidence(t *testing.T) {
	s, p, op, advance := cloneFixture(t)
	advance(50 * time.Millisecond)
	p.Copies = nil
	for _, desired := range [][]Project{{}, {p}, {{ID: "other", Home: Home{Node: "remote", Path: "/clone"}}}} {
		if err := s.ValidateDeclaration(t.Context(), desired); !errors.Is(err, ErrCloneIsolated) {
			t.Fatalf("expired lease bypassed physical isolation: %v", err)
		}
		if err := s.Reconcile(t.Context(), desired, "two"); !errors.Is(err, ErrCloneIsolated) {
			t.Fatalf("transaction reassigned isolated path: %v", err)
		}
	}
	if err := s.ConfirmCloneStopped(t.Context(), op.ID, "operator", ""); err == nil {
		t.Fatal("released isolation without stop evidence")
	}
	if err := s.ConfirmCloneStopped(t.Context(), op.ID, "operator", "node process table confirms clone process exited"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context(), []Project{p}, "two"); err != nil {
		t.Fatal(err)
	}
	records, err := s.l.Events(t.Context(), op.ID)
	if err != nil || len(records) < 3 {
		t.Fatalf("stop confirmation was not audited: %v", err)
	}
	if records[len(records)-1].Actor != "operator" {
		t.Fatal("confirmation actor not recorded")
	}
}

func TestUnknownCloneAndFailedCleanupNeverAutomaticallyReplay(t *testing.T) {
	s, p, op, advance := cloneFixture(t)
	if err := s.FinishClone(t.Context(), op, false, "remote connection closed without a terminal reply", errors.New("transport lost")); err != nil {
		t.Fatal(err)
	}
	active, err := s.CloneOperations(t.Context())
	if err != nil || len(active) != 1 || active[0].State != "unconfirmed" || active[0].Evidence == "" {
		t.Fatalf("uncertainty not durable: %+v %v", active, err)
	}
	advance(50 * time.Millisecond)
	if _, err := s.BeginClone(t.Context(), p.ID, p.Copies["remote"], "", "retry", time.Second); err == nil {
		t.Fatal("unconfirmed clone was replayed")
	}
	if err := s.Retire(t.Context(), p.ID); !errors.Is(err, ErrCloneIsolated) {
		t.Fatalf("unconfirmed clone retired: %v", err)
	}
}

func TestCloneCompletionWriteFailureRetainsIsolation(t *testing.T) {
	s, p, op, advance := cloneFixture(t)
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER clone_finish_failure BEFORE UPDATE ON operations WHEN NEW.kind='workspace-clone' BEGIN SELECT RAISE(ABORT,'completion store failed'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishClone(t.Context(), op, true, "synchronous local process exited", errors.New("clone failed")); err == nil {
		t.Fatal("failed cleanup acknowledged")
	}
	advance(50 * time.Millisecond)
	p.Copies = nil
	if err := s.Reconcile(t.Context(), []Project{p}, "after"); !errors.Is(err, ErrCloneIsolated) {
		t.Fatalf("failed cleanup lost ownership: %v", err)
	}
}

func TestCloneLeaseLossCannotClearPhysicalIsolation(t *testing.T) {
	s, p, op, advance := cloneFixture(t)
	advance(50 * time.Millisecond)
	cause := s.RenewClone(t.Context(), &op, time.Second)
	if !errors.Is(cause, ledger.ErrStale) {
		t.Fatalf("expired clone ownership unexpectedly renewed: %v", cause)
	}
	if err := s.FinishClone(t.Context(), op, false, "lease renewal failed; remote stop not confirmed", cause); err != nil {
		t.Fatal(err)
	}
	p.Copies = nil
	if err := s.Reconcile(t.Context(), []Project{p}, "after"); !errors.Is(err, ErrCloneIsolated) {
		t.Fatalf("lease loss permitted reassignment: %v", err)
	}
	ops, err := s.CloneOperations(t.Context())
	if err != nil || len(ops) != 1 || ops[0].State != "unconfirmed" || ops[0].Evidence == "" || ops[0].Error == "" {
		t.Fatalf("lease-loss isolation lacks recovery evidence: %+v %v", ops, err)
	}
}
