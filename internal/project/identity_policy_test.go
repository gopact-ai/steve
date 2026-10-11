package project

import (
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"testing"
)

func TestLegacyGrantsMigrateOnceWithoutCrossChannelIDInheritance(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	store := Open(book)
	p := Project{ID: "p", Home: Home{Path: t.TempDir()}, DefaultRole: RoleNone, ConfigGrants: map[string]Role{"configured": RoleRead}}
	if err := store.Reconcile(t.Context(), []Project{p}, "identity-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Grant(t.Context(), "p", "same-id", RoleWrite, "owner"); err != nil {
		t.Fatal(err)
	}
	accounts := &identity.Accounts{Book: book}
	if err := accounts.Bootstrap(t.Context(), identity.Configuration{ConsoleOwner: "owner"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.SetIdentityPolicy(t.Context(), accounts, "chat"); err != nil {
			t.Fatal(err)
		}
	}
	native := identity.Account{Channel: "chat", ID: "same-id"}.Principal()
	other := identity.Account{Channel: "console", ID: "same-id"}.Principal()
	role, err := store.Access(t.Context(), "p", native, accounts.Owner)
	if err != nil || role != RoleWrite {
		t.Fatalf("native grant lost: %s %v", role, err)
	}
	role, err = store.Access(t.Context(), "p", other, accounts.Owner)
	if err != nil || role != RoleNone {
		t.Fatalf("equal ID inherited grant: %s %v", role, err)
	}
	audit, err := store.GrantMigrations(t.Context(), "")
	if err != nil || len(audit.Items) != 2 {
		t.Fatalf("migration duplicated or lost config evidence: %+v %v", audit, err)
	}
	for _, m := range audit.Items {
		if m.Original.Principal != m.Account.ID || m.Account.Channel != "chat" {
			t.Fatal("migration lost original native evidence")
		}
	}
}

func TestConflictingConfiguredAliasesCannotIncreaseUnifiedGrant(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	store := Open(book)
	accounts := &identity.Accounts{Book: book}
	if err := accounts.Bootstrap(t.Context(), identity.Configuration{ConsoleOwner: "owner"}); err != nil {
		t.Fatal(err)
	}
	a := identity.Account{Channel: "chat", ID: "a"}
	b := identity.Account{Channel: "other", ID: "b"}
	if _, err := accounts.Bind(t.Context(), b, a.Principal(), 0, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetIdentityPolicy(t.Context(), accounts, "chat"); err != nil {
		t.Fatal(err)
	}
	p := Project{ID: "p", Home: Home{Path: t.TempDir()}, DefaultRole: RoleNone, ConfigGrants: map[string]Role{"account:chat:a": RoleWrite, "account:other:b": RoleRead}}
	// A disabled account must block execution, not prevent configuration
	// reconciliation or erase the grant conflict on the next startup.
	if _, err := accounts.Bind(t.Context(), b, a.Principal(), 1, false); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := store.Reconcile(t.Context(), []Project{p}, "identity-fixture"); err != nil {
			t.Fatal(err)
		}
		role, err := store.Access(t.Context(), "p", a.Principal(), accounts.Owner)
		if err != nil || role != RoleNone {
			t.Fatalf("conflicting aliases escalated grant: %s %v", role, err)
		}
	}
}
