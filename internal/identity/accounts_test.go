package identity

import (
	"errors"
	"github.com/gopact-ai/steve/internal/ledger"
	"testing"
)

func TestEqualNativeIDsHaveSeparateAuthorityAndUnlinkRevokesOriginalExecution(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	accounts := &Accounts{Book: book}
	if err := accounts.Bootstrap(t.Context(), Configuration{ConsoleOwner: "owner"}); err != nil {
		t.Fatal(err)
	}
	console := Account{Channel: "console", ID: "same-id"}
	im := Account{Channel: "chat", ID: "same-id"}
	a, err := accounts.Resolve(t.Context(), console)
	if err != nil {
		t.Fatal(err)
	}
	b, err := accounts.Resolve(t.Context(), im)
	if err != nil {
		t.Fatal(err)
	}
	if a.Principal == b.Principal || a.Principal == accounts.Owner || b.Principal == accounts.Owner {
		t.Fatal("unrelated native accounts inherited authority")
	}
	bound, err := accounts.Bind(t.Context(), im, a.Principal, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return BindExecutionTx(tx, "original", im) }); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Bind(t.Context(), im, "", bound.Revision-1, true); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("stale unlink changed identity: %v", err)
	}
	if _, err := accounts.Bind(t.Context(), im, "", bound.Revision, true); err != nil {
		t.Fatal(err)
	}
	err = book.Read(t.Context(), func(tx *ledger.ReadTx) error { _, err := CheckExecutionTx(tx, "original"); return err })
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("old attempt adopted unlinked account authority: %v", err)
	}
	again, err := accounts.Resolve(t.Context(), console)
	if err != nil || again.Principal != a.Principal {
		t.Fatal("unlink changed the other account")
	}
}

func TestRemovedConfiguredOwnerDoesNotKeepOwnerOrExecutionAuthority(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	accounts := &Accounts{Book: book}
	cfg := Configuration{ConsoleOwner: "owner", NativeOwners: map[string]string{"chat": "native-owner"}}
	if err := accounts.Bootstrap(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	a := Account{Channel: "chat", ID: "native-owner"}
	old, err := accounts.Resolve(t.Context(), a)
	if err != nil || old.Principal != accounts.Owner {
		t.Fatal("configured owner not linked")
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return BindExecutionTx(tx, "original", a) }); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Bootstrap(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := accounts.Resolve(t.Context(), a)
	if unchanged.Revision != old.Revision {
		t.Fatal("unchanged boot revoked execution")
	}
	cfg.NativeOwners = nil
	if err := accounts.Bootstrap(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	removed, err := accounts.Resolve(t.Context(), a)
	if err != nil || removed.Principal == accounts.Owner || removed.Configured {
		t.Fatal("removed declaration retained owner")
	}
	err = book.Read(t.Context(), func(tx *ledger.ReadTx) error { _, err := CheckExecutionTx(tx, "original"); return err })
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("removed owner still executes: %v", err)
	}
}
