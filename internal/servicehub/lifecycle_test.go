package servicehub_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/provider"
)

func TestRetiredConsumerCannotHideEventsOrPreventDisablingItsSubscription(t *testing.T) {
	f := setup(t, "", nil)
	_, producer := enroll(t, f, identity.Grant{Key: "source", Name: "Mail source", Kind: "provider", EventTypes: []string{"mail.*"}})
	consumer, _ := enroll(t, f, identity.Grant{Key: "consumer", Name: "Mail consumer", Kind: "provider"})
	rule, err := f.server.Events.SaveSubscription(t.Context(), events.Subscription{ID: "mail", Name: "Mail", Enabled: true, Mode: "provider", EventType: "mail.*", Handler: consumer.Grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	first := publish(t, f, producer, "first")
	if err := f.server.Events.Route(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Identity.Revoke(t.Context(), consumer.Grant.ID); err != nil {
		t.Fatal(err)
	}
	second := publish(t, f, producer, "second")
	if err := f.server.Events.Route(t.Context(), second.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Events.Pump(t.Context(), f.server.Work); err != nil {
		t.Fatal(err)
	}
	rule.Enabled = false
	if _, err := f.server.Events.SaveSubscription(t.Context(), rule); err != nil {
		t.Fatalf("retired handler prevented disabling its rule: %v", err)
	}
	deliveries, err := f.server.Events.Deliveries(t.Context(), "")
	if err != nil || len(deliveries.Items) != 2 {
		t.Fatalf("deliveries=%+v %v", deliveries, err)
	}
	for _, d := range deliveries.Items {
		if d.State != events.StateCancelled || d.Token != "" {
			t.Fatalf("unclaimed event remained inaccessible: %+v", d)
		}
	}
	inbox, err := f.server.Events.InboxEntries(t.Context(), "")
	if err != nil || len(inbox.Items) != 2 {
		t.Fatalf("retired consumer suppressed fallback: %+v %v", inbox, err)
	}
}

func registeredClient(t *testing.T, f fixture, approval bool) (identity.Enrollment, identity.Principal, clientcap.Registration, clientcap.Request) {
	t.Helper()
	g, p := enroll(t, f, identity.Grant{Key: "mac", Name: "Mac", Kind: "client", Capabilities: []identity.CapabilityGrant{{Capability: "notification.show", Conversation: "console:main", Approval: approval}}})
	reg, err := f.server.Clients.Register(t.Context(), p, clientcap.Register{Capabilities: []clientcap.Capability{{Name: "notification.show", Description: "Show a local notification", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	return g, p, reg, clientcap.Request{Key: "notification", Client: g.Grant.ID, Capability: "notification.show", Conversation: "console:main", Arguments: json.RawMessage(`{"text":"hello"}`)}
}

func TestClientApprovalFreezesContractButAllowsAnIdenticalContractToReconnect(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			f := setup(t, "", nil)
			_, p, reg, req := registeredClient(t, f, true)
			call, err := f.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, req)
			if err != nil {
				t.Fatal(err)
			}
			caps := append([]clientcap.Capability(nil), reg.Capabilities...)
			caps[0].InputSchema = json.RawMessage(`{"properties":{"text":{"type":"string"}},"type":"object"}`)
			if changed {
				caps[0].Description = "Send the contents to a remote account"
			}
			fresh, err := f.server.Clients.Register(t.Context(), p, clientcap.Register{Generation: reg.Generation, Capabilities: caps})
			if err != nil {
				t.Fatal(err)
			}
			err = f.server.Clients.Approve(t.Context(), call.ID, true)
			if changed {
				if !errors.Is(err, clientcap.ErrContractChanged) {
					t.Fatalf("changed contract retained approval: %v", err)
				}
				if err := f.server.Clients.SweepUnclaimed(t.Context()); err != nil {
					t.Fatal(err)
				}
				current, err := f.server.Clients.Status(t.Context(), identity.Principal{Owner: true}, call.ID)
				if err != nil || current.State != clientcap.StateCancelled || current.ContractDigest != call.ContractDigest {
					t.Fatalf("old contract was adopted: %+v %v", current, err)
				}
				if _, found, err := f.server.Clients.Poll(t.Context(), p, fresh.Generation); err != nil || found {
					t.Fatalf("old invocation dispatched to changed client: %v %v", found, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				claimed, found, err := f.server.Clients.Poll(t.Context(), p, fresh.Generation)
				if err != nil || !found || claimed.ID != call.ID || claimed.ClientGeneration != fresh.Generation {
					t.Fatalf("identical contract reconnect failed: %+v %v", claimed, err)
				}
			}
		})
	}
}

func TestClientScopeChangesCannotBypassNewApprovalAndCallerApprovalIsHonored(t *testing.T) {
	f := setup(t, "", nil)
	g, _, _, req := registeredClient(t, f, false)
	call, err := f.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	scopes := append([]identity.CapabilityGrant(nil), g.Grant.Capabilities...)
	scopes[0].Approval = true
	if _, err := f.server.Identity.UpdateScope(t.Context(), g.Grant.ID, identity.ScopeUpdate{Revision: g.Grant.ScopeRevision, Name: g.Grant.Name, Capabilities: scopes}); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Clients.SweepUnclaimed(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := f.server.Clients.Status(t.Context(), identity.Principal{Owner: true}, call.ID)
	if err != nil || current.State != clientcap.StateCancelled {
		t.Fatalf("auto-approved call ignored stricter scope: %+v %v", current, err)
	}
	f2 := setup(t, "", nil)
	g2, _, _, req2 := registeredClient(t, f2, false)
	_, caller := enroll(t, f2, identity.Grant{Key: "automation", Name: "Automation", Kind: "provider", Capabilities: []identity.CapabilityGrant{{Client: g2.Grant.ID, Capability: req2.Capability, Conversation: req2.Conversation, Approval: true}}})
	call, err = f2.server.Clients.Invoke(t.Context(), caller, req2)
	if err != nil || call.State != clientcap.StateApproval || call.CallerPermission == nil || !call.CallerPermission.Approval {
		t.Fatalf("caller approval was ignored: %+v %v", call, err)
	}
}

func TestRevokedClientPendingIsRetiredAndUnknownHasAnEvidenceOnlyReconciliation(t *testing.T) {
	f := setup(t, "", nil)
	g, p, reg, req := registeredClient(t, f, false)
	first, err := f.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.server.Clients.Poll(t.Context(), p, reg.Generation)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		claim.ClaimUntil = time.Now().Add(-time.Second)
		return tx.PutBinding(clientcap.CallKind, claim.ID, claim)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Clients.Recover(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	req.Key = "pending"
	pending, err := f.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.Identity.Revoke(t.Context(), g.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Clients.SweepUnclaimed(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := f.server.Clients.Status(t.Context(), identity.Principal{Owner: true}, pending.ID)
	if err != nil || current.State != clientcap.StateCancelled {
		t.Fatalf("retired client left pending work: %+v %v", current, err)
	}
	unknown, err := f.server.Clients.Status(t.Context(), identity.Principal{Owner: true}, first.ID)
	if err != nil || unknown.State != clientcap.StateUnknown {
		t.Fatal(unknown, err)
	}
	resolution := clientcap.Reconciliation{Revision: unknown.Revision, State: clientcap.StateDone, Receipt: "verified-os-history", Result: json.RawMessage(`{"shown":true}`), Detail: "Owner checked the original notification"}
	for range 2 {
		if err := f.server.Clients.Reconcile(t.Context(), unknown.ID, resolution); err != nil {
			t.Fatal(err)
		}
	}
	settled, err := f.server.Clients.Status(t.Context(), identity.Principal{Owner: true}, unknown.ID)
	if err != nil || settled.State != clientcap.StateDone || settled.Resolution == nil || settled.ClientGeneration != claim.ClientGeneration {
		t.Fatalf("reconciliation lost original evidence: %+v %v", settled, err)
	}
	stored, _, err := durable.Read[clientcap.Invocation](t.Context(), f.book, clientcap.CallKind, unknown.ID)
	if err != nil || stored.Token != claim.Token {
		t.Fatal("original claim token was replaced", err)
	}
	attention, err := f.server.Clients.Attention(t.Context(), "")
	if err != nil || len(attention.Items) != 0 {
		t.Fatalf("settled attention remained: %+v %v", attention, err)
	}
}

func TestUnmatchedInboxBacklogLimitsDoNotRejectTheOriginalIdempotentReceipt(t *testing.T) {
	f := setup(t, "", nil)
	f.server.Events.Limits = durable.Limits{Total: 2, PerSource: 2}
	g, p := enroll(t, f, identity.Grant{Key: "mail", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.*"}})
	first := publish(t, f, p, "one")
	if err := f.server.Events.Route(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	second := publish(t, f, p, "two")
	if err := f.server.Events.Route(t.Context(), second.ID); err != nil {
		t.Fatal(err)
	}
	input := events.Publish{Key: "three", Type: "mail.received", Payload: json.RawMessage(`{}`)}
	if _, err := f.server.Events.Publish(t.Context(), p, input); !errors.Is(err, durable.ErrCapacity) {
		t.Fatalf("unmatched backlog bypassed quota: %v", err)
	}
	if _, found, err := durable.Read[ingress.Receipt](t.Context(), f.book, ingress.RecordKind, durable.Key("event", g.Grant.Principal, input.Key)); err != nil || found {
		t.Fatal("refused admission retained ingress", err)
	}
	replay := publish(t, f, p, "one")
	if replay.ID != first.ID {
		t.Fatal("capacity rejection changed original receipt")
	}
	inbox, err := f.server.Events.InboxEntries(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.Events.ResolveInbox(t.Context(), inbox.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.Events.Publish(t.Context(), p, input); err != nil {
		t.Fatalf("resolved fallback did not release capacity: %v", err)
	}
}

func TestProviderContractUpdateRetainsTheAcceptedEventAndRejectsChangedReplay(t *testing.T) {
	f := setup(t, "", nil)
	_, p := enroll(t, f, identity.Grant{Key: "mail", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.*"}})
	reg, err := f.server.Providers.Register(t.Context(), p, provider.Register{PluginID: "mail", Version: "1", Events: []provider.Contract{{Type: "mail.received", Version: "1", Schema: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	first := publish(t, f, p, "message")
	if first.Contract == nil || first.Contract.Version != "1" {
		t.Fatal("accepted event lost contract", first)
	}
	_, err = f.server.Providers.Register(t.Context(), p, provider.Register{Generation: reg.Generation, PluginID: "mail", Version: "2", Events: []provider.Contract{{Type: "mail.received", Version: "2", Schema: json.RawMessage(`{"type":"object","required":["subject"]}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	replay := publish(t, f, p, "message")
	if replay.Contract == nil || replay.Contract.Digest != first.Contract.Digest {
		t.Fatal("replay adopted the updated contract")
	}
	if _, err := f.server.Events.Publish(t.Context(), p, events.Publish{Key: "message", Type: first.Type, Version: "2", Payload: first.Payload}); !errors.Is(err, ingress.ErrConflict) {
		t.Fatalf("changed version reused original identity: %v", err)
	}
}
