package project

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestReleaseCannotBeUndoneByConfiguredDeclarationOrRestart(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := Open(book)
	s.SetHubID("hub-a")
	p := Project{ID: "p", Home: Home{Path: t.TempDir()}}
	if err := s.Declare(t.Context(), []Project{p}); err != nil {
		t.Fatal(err)
	}
	released, err := s.Release(t.Context(), p.ID, "hub-b", "transfer-1", "all writers verified stopped", func(*ledger.Tx, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if released.Epoch != 2 || released.State != "released" {
		t.Fatalf("release=%+v", released)
	}
	reopened := Open(book)
	reopened.SetHubID("hub-a")
	if err := reopened.Declare(t.Context(), []Project{p}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.Get(t.Context(), p.ID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("source reactivated: %v", err)
	}
	if _, ok, err := reopened.Lookup(t.Context(), p.ID); err != nil || !ok {
		t.Fatal("released history disappeared", err)
	}
	if _, err := reopened.Materialize(t.Context(), Request{Project: p.ID}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("released project materialized: %v", err)
	}
}
func TestAcceptChecksTargetAndRejectsCollisionAndStaleReplay(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := Open(book)
	s.SetHubID("hub-b")
	p := Project{ID: "p", Home: Home{Path: t.TempDir()}}
	release := Ownership{Project: p.ID, HubID: "hub-a", TargetHub: "hub-c", Epoch: 2, State: "released", TransferID: "transfer"}
	if err := s.Accept(t.Context(), p, release); err == nil {
		t.Fatal("wrong hub accepted")
	}
	release.TargetHub = "hub-b"
	if err := s.Accept(t.Context(), p, release); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(t.Context(), p, release); err != nil {
		t.Fatal("same transfer not idempotent", err)
	}
	release.TransferID = "another"
	if err := s.Accept(t.Context(), p, release); err == nil {
		t.Fatal("same epoch different transfer accepted")
	}
}
func TestReleaseRequiresQuiescenceGuard(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := Open(book)
	s.SetHubID("a")
	if err := s.Declare(t.Context(), []Project{{ID: "p", Home: Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("active writer")
	if _, err := s.Release(context.Background(), "p", "b", "tx", "claimed stopped", func(*ledger.Tx, string) error { return cause }); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if _, _, err := s.Get(t.Context(), "p"); err != nil {
		t.Fatal("failed release changed authority", err)
	}
}
