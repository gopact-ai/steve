package identity

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

func connectionFixture(t *testing.T) *Service {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return &Service{Book: book, OwnerID: "owner"}
}

func TestCredentialRecoveryRetainsIdentityAndFencesAuthenticatedRequests(t *testing.T) {
	s := connectionFixture(t)
	declaration := Grant{Key: "mail-installation", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.*"}, Work: map[string]string{"console:mail": "scratch"}}
	initial, err := s.Enroll(t.Context(), declaration)
	if err != nil {
		t.Fatal(err)
	}
	oldCaller, err := s.Authenticate(t.Context(), initial.Token)
	if err != nil {
		t.Fatal(err)
	}
	rotate := RotateRequest{Revision: initial.Grant.CredentialRevision, Key: "recover-response"}
	updated, err := s.RotateCredential(t.Context(), initial.Grant.ID, rotate)
	if err != nil || updated.Token == "" || updated.Grant.ID != initial.Grant.ID || updated.Grant.Principal != initial.Grant.Principal || updated.Grant.ScopeRevision != initial.Grant.ScopeRevision {
		t.Fatalf("rotation did not retain its logical connection: %v", err)
	}
	if _, err := s.Authenticate(t.Context(), initial.Token); !errors.Is(err, ErrDenied) {
		t.Fatalf("old credential survived rotation: %v", err)
	}
	err = s.Book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := s.Check(tx, oldCaller); return err })
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("request authenticated before rotation retained write authority: %v", err)
	}
	current, err := s.Authenticate(t.Context(), updated.Token)
	if err != nil || current.GrantID != oldCaller.GrantID {
		t.Fatalf("replacement credential changed the caller: %v", err)
	}
	err = s.Book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		_, err := s.Check(tx, Principal{GrantID: initial.Grant.ID})
		return err
	})
	if err != nil {
		t.Fatalf("accepted background work was invalidated by token recovery: %v", err)
	}
	replayed, err := s.RotateCredential(t.Context(), initial.Grant.ID, rotate)
	if err != nil || replayed.Token != "" || replayed.Grant.CredentialRevision != updated.Grant.CredentialRevision {
		t.Fatalf("lost rotation response was not idempotent: %v", err)
	}
	if _, err := s.RotateCredential(t.Context(), initial.Grant.ID, RotateRequest{Revision: rotate.Revision, Key: "stale-observer"}); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("stale observer rotated another credential: %v", err)
	}
	if _, err := s.RotateCredential(t.Context(), initial.Grant.ID, RotateRequest{Revision: updated.Grant.CredentialRevision, Key: "recover-response"}); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("reused operation key changed its CAS input: %v", err)
	}
	recovery, err := s.RotateCredential(t.Context(), initial.Grant.ID, RotateRequest{Revision: updated.Grant.CredentialRevision, Key: "recover-again"})
	if err != nil || recovery.Token == "" || recovery.Grant.Principal != initial.Grant.Principal {
		t.Fatalf("lost secret required a replacement installation: %v", err)
	}
	enrolledAgain, err := s.Enroll(t.Context(), declaration)
	if err != nil || enrolledAgain.Token != "" || enrolledAgain.Grant.ID != initial.Grant.ID {
		t.Fatalf("original enrollment receipt changed after rotation: %v", err)
	}
	audit, err := s.Audit(t.Context(), "")
	if err != nil || len(audit.Items) != 3 {
		t.Fatalf("credential audit missing: %d %v", len(audit.Items), err)
	}
	raw, _ := json.Marshal(audit)
	for _, secret := range []string{initial.Token, updated.Token, recovery.Token} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("credential audit contains a bearer secret")
		}
	}
}

func TestLegacyCredentialsRotateWithoutChangingTheirAcceptedSource(t *testing.T) {
	s := connectionFixture(t)
	legacy := Grant{ID: "legacy-mail", Key: "old-installation", Name: "Mail", Kind: "provider", Principal: "connection:legacy-mail", Enabled: true, EventTypes: []string{"mail.*"}}
	secret := "legacy-bearer"
	if err := s.Book.Update(t.Context(), func(tx *ledger.Tx) error {
		if err := tx.PutBinding(GrantKind, legacy.ID, legacy); err != nil {
			return err
		}
		return tx.PutBinding(credentialKind, durable.Key(secret), legacy.ID)
	}); err != nil {
		t.Fatal(err)
	}
	caller, err := s.Authenticate(t.Context(), secret)
	if err != nil || !caller.Authenticated || caller.CredentialRevision != 0 {
		t.Fatalf("legacy credential was not recognized: %v", err)
	}
	rotated, err := s.RotateCredential(t.Context(), legacy.ID, RotateRequest{Key: "upgrade"})
	if err != nil || rotated.Grant.ID != legacy.ID || rotated.Grant.CredentialRevision != 1 {
		t.Fatalf("legacy rotation failed: %v", err)
	}
	err = s.Book.Read(t.Context(), func(tx *ledger.ReadTx) error { _, err := s.Check(tx, caller); return err })
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("legacy request escaped the generation fence: %v", err)
	}
	if _, err := s.Authenticate(t.Context(), secret); !errors.Is(err, ErrDenied) {
		t.Fatalf("legacy credential lookup survived rotation: %v", err)
	}
}

func TestScopeEditPreservesEnrollmentAndCannotRecoverRetiredAuthority(t *testing.T) {
	s := connectionFixture(t)
	declaration := Grant{Key: "mail", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.*"}, Work: map[string]string{"console:mail": "scratch"}}
	enrolled, err := s.Enroll(t.Context(), declaration)
	if err != nil {
		t.Fatal(err)
	}
	input := ScopeUpdate{Revision: enrolled.Grant.ScopeRevision, Name: "Updated mail", EventTypes: []string{"mail.received"}}
	updated, err := s.UpdateScope(t.Context(), enrolled.Grant.ID, input)
	if err != nil || updated.ID != enrolled.Grant.ID || updated.Principal != enrolled.Grant.Principal || updated.CredentialRevision != enrolled.Grant.CredentialRevision || updated.AllowsWork("console:mail", "scratch") {
		t.Fatalf("scope edit changed identity or retained removed delegation: %v", err)
	}
	retry, err := s.UpdateScope(t.Context(), enrolled.Grant.ID, input)
	if err != nil || retry.ScopeRevision != updated.ScopeRevision {
		t.Fatalf("scope edit lost-response retry created another revision: %v", err)
	}
	input.Name = "Stale update"
	if _, err := s.UpdateScope(t.Context(), enrolled.Grant.ID, input); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("stale scope update accepted: %v", err)
	}
	replay, err := s.Enroll(t.Context(), declaration)
	if err != nil || replay.Grant.Name != updated.Name || replay.Token != "" {
		t.Fatalf("original enrollment receipt no longer resolves updated grant: %v", err)
	}
	if err := s.Revoke(t.Context(), enrolled.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateCredential(t.Context(), enrolled.Grant.ID, RotateRequest{Revision: updated.CredentialRevision, Key: "cannot-reactivate"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("credential rotation reactivated a retired provider: %v", err)
	}
	input.Revision = updated.ScopeRevision + 1
	if _, err := s.UpdateScope(t.Context(), enrolled.Grant.ID, input); !errors.Is(err, ErrDenied) {
		t.Fatalf("scope update reactivated a retired provider: %v", err)
	}
	if _, err := s.Authenticate(t.Context(), enrolled.Token); !errors.Is(err, ErrDenied) {
		t.Fatalf("retired token authenticated: %v", err)
	}
}
