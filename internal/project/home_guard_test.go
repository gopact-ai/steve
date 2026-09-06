package project

import (
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestHomeAdmissionRejectsChangedOrRetiredDeclaration(t *testing.T) {
	s := openStore(t)
	original := Home{Node: "node", Path: "/original"}
	p := Project{ID: "sample", Home: original}
	if err := s.Declare(t.Context(), []Project{p}); err != nil {
		t.Fatal(err)
	}
	check := func() error {
		return s.l.Update(t.Context(), func(tx *ledger.Tx) error { return CheckHomeTx(tx, p.ID, original) })
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	p.Home.Path = "/replacement"
	if err := s.Declare(t.Context(), []Project{p}); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("admitted a stale physical target")
	}
	if err := s.Reconcile(t.Context(), nil, "retired"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("admitted a retired target")
	}
}
