package provider

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
)

func providerFixture(t *testing.T) (*Service, identity.Enrollment, identity.Principal) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	id := &identity.Service{Book: book, OwnerID: "owner"}
	enrolled, err := id.Enroll(t.Context(), identity.Grant{Key: "mail", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.*"}})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := id.Authenticate(t.Context(), enrolled.Token)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Book: book, Identity: id}, enrolled, caller
}

func TestProviderContractsAreImmutableAndDiscoverySurvivesCredentialRecovery(t *testing.T) {
	s, enrolled, caller := providerFixture(t)
	input := Register{PluginID: "mail.host", Version: "1.0.0", Events: []Contract{{Type: "mail.received", Version: "1", Description: "New message", Schema: json.RawMessage(`{"type":"object","properties":{"subject":{"type":"string"}}}`)}}}
	registered, err := s.Register(t.Context(), caller, input)
	if err != nil || registered.ID != enrolled.Grant.ID || registered.Principal != enrolled.Grant.Principal || registered.Events[0].Digest == "" {
		t.Fatalf("provider registration changed its identity: %v", err)
	}
	input.Events[0].Schema = json.RawMessage(`{ "properties": { "subject": { "type": "string" } }, "type": "object" }`)
	retry, err := s.Register(t.Context(), caller, input)
	if err != nil || retry.Generation != registered.Generation {
		t.Fatalf("formatting-only lost-response retry changed contract: %v", err)
	}
	rotated, err := s.Identity.RotateCredential(t.Context(), enrolled.Grant.ID, identity.RotateRequest{Revision: enrolled.Grant.CredentialRevision, Key: "restore-key"})
	if err != nil {
		t.Fatal(err)
	}
	newCaller, err := s.Identity.Authenticate(t.Context(), rotated.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(t.Context(), newCaller, registered.Generation); err != nil {
		t.Fatalf("token recovery required replacing provider registration: %v", err)
	}
	input.Generation = registered.Generation
	input.Events[0].Schema = json.RawMessage(`{"type":"object","required":["subject"]}`)
	if _, err := s.Register(t.Context(), newCaller, input); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("same event version silently changed contract: %v", err)
	}
	input.Events[0].Version = "2"
	updated, err := s.Register(t.Context(), newCaller, input)
	if err != nil || updated.Events[0].Digest == registered.Events[0].Digest {
		t.Fatalf("new contract version could not be registered: %v", err)
	}
	err = s.Book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		_, _, err := Freeze(tx, enrolled.Grant.ID, "mail.received", "1")
		return err
	})
	if !errors.Is(err, ErrContract) {
		t.Fatalf("withdrawn old contract remained advertised: %v", err)
	}
	input.Generation = updated.Generation
	input.Events = nil
	empty, err := s.Register(t.Context(), newCaller, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Generation = empty.Generation
	input.Events = []Contract{{Type: "mail.received", Version: "1", Schema: json.RawMessage(`{"type":"object"}`)}}
	if _, err := s.Register(t.Context(), newCaller, input); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("removing a declaration erased its immutable contract history: %v", err)
	}
}

func TestProviderHealthAndScopedDeclarationsDoNotGrantExecution(t *testing.T) {
	s, enrolled, caller := providerFixture(t)
	assertHealth := func(want string) {
		t.Helper()
		err := s.Book.Read(t.Context(), func(tx *ledger.ReadTx) error {
			health, err := HealthTx(tx, enrolled.Grant.ID)
			if err != nil {
				return err
			}
			if health.State != want {
				t.Fatalf("health=%q want %q", health.State, want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertHealth("unregistered")
	input := Register{PluginID: "mail.host", Version: "1", Events: []Contract{{Type: "calendar.changed", Version: "1", Schema: json.RawMessage(`{}`)}}}
	if _, err := s.Register(t.Context(), caller, input); !errors.Is(err, identity.ErrDenied) {
		t.Fatalf("registration expanded owner-delegated event scope: %v", err)
	}
	input.Events[0].Type = "mail.received"
	registration, err := s.Register(t.Context(), caller, input)
	if err != nil {
		t.Fatal(err)
	}
	assertHealth("online")
	registration.Until = time.Now().Add(-time.Minute)
	if err := s.Book.PutBinding(t.Context(), RegistrationKind, enrolled.Grant.ID, registration); err != nil {
		t.Fatal(err)
	}
	assertHealth("offline")
	if err := s.Heartbeat(t.Context(), caller, registration.Generation-1); !errors.Is(err, ledger.ErrStale) {
		t.Fatalf("old provider process revived current health: %v", err)
	}
	if err := s.Heartbeat(t.Context(), caller, registration.Generation); err != nil {
		t.Fatal(err)
	}
	assertHealth("online")
	if err := s.Identity.Revoke(t.Context(), enrolled.Grant.ID); err != nil {
		t.Fatal(err)
	}
	assertHealth("retired")
	if err := s.Heartbeat(t.Context(), caller, registration.Generation); !errors.Is(err, identity.ErrDenied) {
		t.Fatalf("retired provider revived availability: %v", err)
	}
	legacy, err := s.Identity.Enroll(t.Context(), identity.Grant{Key: "legacy", Name: "Legacy", Kind: "provider", EventTypes: []string{"mail.*"}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		_, declared, err := Freeze(tx, legacy.Grant.ID, "mail.received", "")
		if declared {
			t.Fatal("legacy provider requires a retroactive declaration")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = durable.Read[Registration](t.Context(), s.Book, RegistrationKind, legacy.Grant.ID)
	if err != nil {
		t.Fatal(err)
	}
}
