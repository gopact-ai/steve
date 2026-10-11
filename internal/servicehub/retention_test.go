package servicehub_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/work"
)

func TestCompactionRetainsRejectedWorkWithoutInventingAnExecution(t *testing.T) {
	f := setup(t, "", nil)
	g, p := enroll(t, f, identity.Grant{Key: "rejected-work", Name: "Provider", Kind: "provider", Work: map[string]string{"console:main": "scratch"}})
	request := work.Request{Key: "original", Conversation: "console:main", Project: "scratch", Instruction: "Process mail", Context: json.RawMessage(`{"body":"` + strings.Repeat("original mail ", 1000) + `"}`)}
	accepted, err := f.server.Work.Submit(t.Context(), p, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.Identity.Revoke(t.Context(), g.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Work.Dispatch(t.Context(), accepted.ID); err != nil {
		t.Fatal(err)
	}
	before, _, err := durable.Read[work.Record](t.Context(), f.book, work.RecordKind, accepted.ID)
	if err != nil || before.State != work.StateFailed || before.Outcome != nil || before.Exchange != "" || before.Detail == "" {
		t.Fatalf("refusal lost its original receipt: %+v %v", before, err)
	}
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE bindings SET updated_at=? WHERE kind=? AND id=?`, old, work.RecordKind, accepted.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if compacted, err := f.server.Compact(t.Context()); err != nil || compacted.Archived != 1 {
		t.Fatalf("rejected Work was left permanently hot: %+v %v", compacted, err)
	}
	after, result, err := f.server.Work.Status(t.Context(), identity.Principal{Owner: true}, accepted.ID)
	if err != nil || !reflect.DeepEqual(before, after) || result.Exchange != "" || f.runner.calls.Load() != 0 {
		t.Fatalf("cold refusal changed facts or ran an execution: %+v %v", after, err)
	}
}

func TestLosslessCompactionRetainsResultsAndDoesNotArchiveUnknownOrUnresolvedEvents(t *testing.T) {
	f := setup(t, "", nil)
	_, p, reg, req := registeredClient(t, f, false)
	first, err := f.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.server.Clients.Poll(t.Context(), p, reg.Generation)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	result, _ := json.Marshal(map[string]string{"text": strings.Repeat("original completed output\n", 1000)})
	if err := f.server.Clients.Complete(t.Context(), p, first.ID, clientcap.Completion{Token: claim.Token, State: clientcap.StateDone, Receipt: "original-result", Result: result}); err != nil {
		t.Fatal(err)
	}
	req.Key = "unknown"
	second, err := f.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	claim2, found, err := f.server.Clients.Poll(t.Context(), p, reg.Generation)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := f.server.Clients.Complete(t.Context(), p, second.ID, clientcap.Completion{Token: claim2.Token, State: clientcap.StateUnknown, Error: "original effect is uncertain", Result: result}); err != nil {
		t.Fatal(err)
	}
	_, provider := enroll(t, f, identity.Grant{Key: "mail", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.*"}})
	openEvent, err := f.server.Events.Publish(t.Context(), provider, events.Publish{Key: "unhandled", Type: "mail.received", Payload: result})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.Events.Route(t.Context(), openEvent.ID); err != nil {
		t.Fatal(err)
	}
	before, _, err := durable.Read[clientcap.Invocation](t.Context(), f.book, clientcap.CallKind, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE bindings SET updated_at=? WHERE (kind=? AND id IN (?,?)) OR (kind=? AND id=?)`, old, clientcap.CallKind, first.ID, second.ID, events.EventKind, openEvent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	compacted, err := f.server.Compact(t.Context())
	if err != nil || compacted.Archived != 1 || compacted.SavedBytes <= 0 {
		t.Fatalf("compaction=%+v %v", compacted, err)
	}
	after, _, err := durable.Read[clientcap.Invocation](t.Context(), f.book, clientcap.CallKind, first.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("cold storage changed original result/proof: %v", err)
	}
	for _, record := range []struct{ kind, id string }{{clientcap.CallKind, second.ID}, {events.EventKind, openEvent.ID}} {
		var pointer string
		if err := f.book.DB().QueryRow(`SELECT coalesce(json_extract(data,'$._steve_cold_record'),'') FROM bindings WHERE kind=? AND id=?`, record.kind, record.id).Scan(&pointer); err != nil || pointer != "" {
			t.Fatalf("unfinished evidence was archived: %+v %s %v", record, pointer, err)
		}
	}
	stats, err := f.server.Stats(t.Context())
	if err != nil || stats.Storage.ColdRecords != 1 || stats.Queues["client_unknown"].Pending != 1 || stats.Queues["event_inbox"].Pending != 1 {
		t.Fatalf("stats=%+v %v", stats, err)
	}
	replay, err := f.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, clientcap.Request{Key: first.Request.Key, Client: first.Request.Client, Capability: first.Request.Capability, Conversation: first.Request.Conversation, Arguments: first.Request.Arguments})
	if err != nil || replay.ID != first.ID || replay.State != clientcap.StateDone || string(replay.Result) != string(result) {
		t.Fatalf("cold result lost idempotency: %+v %v", replay, err)
	}
}

func TestCapabilityDiscoveryFiltersHistoricalOfflineAndUnauthorizedClientsBeforeItsLimit(t *testing.T) {
	f := setup(t, "", nil)
	live, _, _, req := registeredClient(t, f, false)
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		for n := range 205 {
			id := fmt.Sprintf("%064x", n)
			until := time.Now().UTC().Add(-time.Hour)
			conversation := req.Conversation
			if n%2 == 0 {
				until = time.Now().UTC().Add(time.Hour)
				conversation = "console:other"
			}
			grant := identity.Grant{ID: id, Name: "Old", Kind: "client", Enabled: true, Capabilities: []identity.CapabilityGrant{{Client: id, Capability: req.Capability, Conversation: conversation}}}
			registration := clientcap.Registration{ID: id, Name: "Old", Generation: 1, Until: until, Capabilities: []clientcap.Capability{{Name: req.Capability, InputSchema: json.RawMessage(`{"type":"object"}`)}}}
			if err := tx.PutBinding(identity.GrantKind, id, grant); err != nil {
				return err
			}
			if err := tx.PutBinding(clientcap.ClientKind, id, registration); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	available, err := f.server.Clients.Available(t.Context(), identity.Principal{Owner: true}, req.Conversation)
	if err != nil || len(available) != 1 || available[0].ID != live.Grant.ID {
		t.Fatalf("history hid the live client: %+v %v", available, err)
	}
	_, caller := enroll(t, f, identity.Grant{Key: "caller", Name: "Scoped caller", Kind: "provider", Capabilities: []identity.CapabilityGrant{{Client: live.Grant.ID, Capability: req.Capability, Conversation: req.Conversation}}})
	available, err = f.server.Clients.Available(t.Context(), caller, req.Conversation)
	if err != nil || len(available) != 1 || available[0].ID != live.Grant.ID {
		t.Fatalf("caller scope discovery failed: %+v %v", available, err)
	}
}

func TestColdEventReactivationRestoresItsOriginalSourceQuota(t *testing.T) {
	for _, mode := range []string{"reprocess", "failed_retry"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, "", nil)
			f.server.Events.Limits = durable.Limits{Total: 10, PerSource: 1}
			_, producer := enroll(t, f, identity.Grant{Key: "producer", Name: "Mail producer", Kind: "provider", EventTypes: []string{"mail.*"}})
			var consumer identity.Principal
			if mode == "failed_retry" {
				g, p := enroll(t, f, identity.Grant{Key: "consumer", Name: "Mail consumer", Kind: "provider"})
				consumer = p
				if _, err := f.server.Events.SaveSubscription(t.Context(), events.Subscription{ID: "mail", Name: "Mail", Enabled: true, Mode: "provider", EventType: "mail.*", Handler: g.Grant.ID}); err != nil {
					t.Fatal(err)
				}
			}
			payload, _ := json.Marshal(map[string]string{"body": strings.Repeat("original frozen mail payload\n", 1000)})
			input := events.Publish{Key: "original", Type: "mail.received", Payload: payload}
			original, err := f.server.Events.Publish(t.Context(), producer, input)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.server.Events.Route(t.Context(), original.ID); err != nil {
				t.Fatal(err)
			}
			var deliveryID string
			if mode == "failed_retry" {
				claim, found, err := f.server.Events.Poll(t.Context(), consumer)
				if err != nil || !found {
					t.Fatal(found, err)
				}
				deliveryID = claim.Delivery.ID
				if err := f.server.Events.Ack(t.Context(), consumer, deliveryID, events.Acknowledgement{Token: claim.Delivery.Token, State: events.StateFailed, Receipt: "original-failure", Detail: "mail handler failed"}); err != nil {
					t.Fatal(err)
				}
			}
			inbox, err := f.server.Events.InboxEntries(t.Context(), "")
			if err != nil || len(inbox.Items) != 1 {
				t.Fatal(inbox, err)
			}
			if err := f.server.Events.ResolveInbox(t.Context(), inbox.Items[0].ID); err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
			if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec(`UPDATE bindings SET updated_at=? WHERE kind=? AND id=?`, old, events.EventKind, original.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if compacted, err := f.server.Compact(t.Context()); err != nil || compacted.Archived != 1 {
				t.Fatalf("compaction=%+v %v", compacted, err)
			}
			var pointer string
			if err := f.book.DB().QueryRow(`SELECT coalesce(json_extract(data,'$._steve_cold_record'),'') FROM bindings WHERE kind=? AND id=?`, events.EventKind, original.ID).Scan(&pointer); err != nil || pointer == "" {
				t.Fatalf("expected a cold original event: %s %v", pointer, err)
			}
			if mode == "failed_retry" {
				err = f.server.Events.Retry(t.Context(), deliveryID)
			} else {
				err = f.server.Events.Reprocess(t.Context(), original.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.book.DB().QueryRow(`SELECT coalesce(json_extract(data,'$._steve_cold_record'),'') FROM bindings WHERE kind=? AND id=?`, events.EventKind, original.ID).Scan(&pointer); err != nil || pointer != "" {
				t.Fatalf("unfinished event retained a cold pointer: %s %v", pointer, err)
			}
			replay, err := f.server.Events.Publish(t.Context(), producer, input)
			if err != nil || !reflect.DeepEqual(original, replay) {
				t.Fatalf("reactivation changed the original receipt: %v", err)
			}
			input.Key = "new-mail"
			if _, err := f.server.Events.Publish(t.Context(), producer, input); !errors.Is(err, durable.ErrCapacity) {
				t.Fatalf("reactivated event evaded its source quota: %v", err)
			}
			if _, err := f.server.Compact(t.Context()); err != nil {
				t.Fatal(err)
			}
			stats, err := f.server.Stats(t.Context())
			if err != nil || stats.Storage.ColdRecords != 0 {
				t.Fatalf("orphaned cold copy remained after hot restoration: %+v %v", stats.Storage, err)
			}
		})
	}
}
