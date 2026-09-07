package project

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestReconcileRetiresOnlyActiveMetadataAndPreservesHistory(t *testing.T) {
	s := openStore(t)
	old := Project{ID: "old", Home: Home{Path: "/old"}}
	if err := s.Reconcile(t.Context(), []Project{old}, "before"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Begin(t.Context(), "historic-attempt", "attempt", "done", "test", map[string]string{"project": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutBinding(t.Context(), "artifact", "historic-artifact", map[string]string{"project": "old"}); err != nil {
		t.Fatal(err)
	}
	s.RequireDeclaration("after")
	if err := s.Reconcile(t.Context(), []Project{{ID: "new", Home: Home{Path: "/new"}}}, "after"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get(t.Context(), "old"); err != nil || ok {
		t.Fatalf("retired project stayed active: %v %v", ok, err)
	}
	historical, ok, err := s.GetHistorical(t.Context(), "old")
	if err != nil || !ok || historical.Home.Path != "/old" {
		t.Fatalf("historical metadata lost: %+v %v", historical, err)
	}
	if _, err := s.Materialize(t.Context(), Request{Project: "old"}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("retired project admitted work: %v", err)
	}
	if _, err := s.Bind(t.Context(), "conversation", "old", "test"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("retired project accepted binding: %v", err)
	}
	var artifact map[string]string
	if ok, err := s.l.GetBinding(t.Context(), "artifact", "historic-artifact", &artifact); err != nil || !ok {
		t.Fatalf("history artifact lost: %v", err)
	}
	if _, ok, err := s.l.Operation(t.Context(), "historic-attempt"); err != nil || !ok {
		t.Fatalf("history attempt lost: %v", err)
	}
}

func TestReconcileFailureKeepsWholePriorProjectionAndGatesLiveUse(t *testing.T) {
	s := openStore(t)
	if err := s.Reconcile(t.Context(), []Project{{ID: "old", Home: Home{Path: "/old"}}}, "before"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_reconcile BEFORE INSERT ON bindings WHEN NEW.kind = 'project-declarations' BEGIN SELECT RAISE(ABORT, 'reconcile failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s.RequireDeclaration("after")
	if err := s.Reconcile(t.Context(), []Project{{ID: "new", Home: Home{Path: "/new"}}}, "after"); err == nil {
		t.Fatal("failed transaction acknowledged")
	}
	state, err := s.AppliedDeclaration(t.Context())
	if err != nil || state.Hash != "before" {
		t.Fatalf("partial revision committed: %+v %v", state, err)
	}
	if _, _, err := s.Get(t.Context(), "old"); !errors.Is(err, ErrDeclarationPending) {
		t.Fatalf("stale projection remained live: %v", err)
	}
	if _, ok, err := s.GetHistorical(t.Context(), "old"); err != nil || !ok {
		t.Fatalf("rollback lost old projection: %v", err)
	}
	if _, ok, err := s.GetHistorical(t.Context(), "new"); err != nil || ok {
		t.Fatalf("transaction partially wrote new projection: %v", err)
	}
}

func TestReconcilePreservesDeclaredCopyProgressAndRejectsLateClone(t *testing.T) {
	s := openStore(t)
	copy := Copy{Node: "remote", Path: "/copy", Origin: OriginCloned, Source: "repository", State: CopyProvisioning}
	p := Project{ID: "p", Home: Home{Path: "/home"}, Copies: map[string]Copy{"remote": copy}}
	if err := s.Reconcile(t.Context(), []Project{p}, "one"); err != nil {
		t.Fatal(err)
	}
	copy.State, copy.Error = CopyFailed, "clone refused"
	if err := s.UpdateDeclaredCopy(t.Context(), "p", copy); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context(), []Project{p}, "one"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get(t.Context(), "p")
	if got.Copies["remote"].State != CopyFailed {
		t.Fatal("restart replaced observed clone state")
	}
	p.Copies = nil
	if err := s.Reconcile(t.Context(), []Project{p}, "two"); err != nil {
		t.Fatal(err)
	}
	copy.State = CopyReady
	if err := s.UpdateDeclaredCopy(t.Context(), "p", copy); !errors.Is(err, ErrUnknown) {
		t.Fatalf("late clone recreated removed copy: %v", err)
	}
	got, _, _ = s.Get(t.Context(), "p")
	if len(got.Copies) != 0 {
		t.Fatal("late clone resurrected copy")
	}
}
