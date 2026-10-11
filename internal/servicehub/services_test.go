package servicehub_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/serviceapi"
	"github.com/gopact-ai/steve/internal/servicehub"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"github.com/gopact-ai/steve/internal/work"
)

type coordinator struct {
	turntest.IdleCoordinator
	calls atomic.Int32
}

func (*coordinator) Context(context.Context, string) (turn.Context, error) {
	return turn.Context{Project: &turn.ContextProject{ID: "scratch", Bound: true}}, nil
}
func (c *coordinator) Handle(_ context.Context, r turn.Request) (turn.Result, error) {
	c.calls.Add(1)
	return turn.Result{Text: "Processed the original event"}, nil
}

type fixture struct {
	book         *ledger.Ledger
	server       *servicehub.Services
	conversation *console.Service
	runner       *coordinator
	dir          string
}

func setup(t *testing.T, dir string, runner *coordinator) fixture {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	if runner == nil {
		runner = &coordinator{}
	}
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	conversation := console.New(runner, "owner", nil)
	if err := conversation.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conversation.Shutdown(context.Background()); _ = book.Close() })
	return fixture{book, servicehub.New(book, "owner", conversation), conversation, runner, dir}
}
func enroll(t *testing.T, f fixture, g identity.Grant) (identity.Enrollment, identity.Principal) {
	t.Helper()
	out, err := f.server.Identity.Enroll(t.Context(), g)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.server.Identity.Authenticate(t.Context(), out.Token)
	if err != nil {
		t.Fatal(err)
	}
	return out, p
}
func publish(t *testing.T, f fixture, p identity.Principal, key string) events.Event {
	t.Helper()
	e, err := f.server.Events.Publish(t.Context(), p, events.Publish{Key: key, Type: "mail.received", Payload: json.RawMessage(`{"subject":"Hello","message_id":"message-1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestUnhandledEventCanBeSubscribedAndReprocessedWithoutResettingItsReceipt(t *testing.T) {
	f := setup(t, "", nil)
	enrollment, p := enroll(t, f, identity.Grant{Key: "mail", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.*"}})
	e := publish(t, f, p, "message-1")
	if err := f.server.Events.Route(t.Context(), e.ID); err != nil {
		t.Fatal(err)
	}
	inbox, err := f.server.Events.InboxEntries(t.Context(), "")
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].Reason != "unhandled" {
		t.Fatalf("fallback=%+v %v", inbox, err)
	}
	rule := events.Subscription{ID: "mail-rule", Name: "Handle mail", EventType: "mail.received", Enabled: true, Mode: "provider", Handler: enrollment.Grant.ID}
	saved, err := f.server.Events.SaveSubscription(t.Context(), rule)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.server.Events.SaveSubscription(t.Context(), rule)
	if err != nil || replay.Revision != saved.Revision {
		t.Fatalf("lost save response=%+v %v", replay, err)
	}
	for range 2 {
		if err := f.server.Events.Reprocess(t.Context(), e.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.server.Events.Route(t.Context(), e.ID); err != nil {
			t.Fatal(err)
		}
	}
	claimed, found, err := f.server.Events.Poll(t.Context(), p)
	if err != nil || !found {
		t.Fatalf("claim=%+v %v", claimed, err)
	}
	// Editing a subscription cannot retarget a delivery already accepted by it.
	saved.Name = "Edited rule"
	saved.ReplaySafe = true
	if _, err := f.server.Events.SaveSubscription(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	if claimed.Delivery.Rule.Name != "Handle mail" || claimed.Delivery.Rule.ReplaySafe {
		t.Fatal("delivery lost its frozen policy")
	}
	ack := events.Acknowledgement{Token: claimed.Delivery.Token, State: "done", Receipt: "mail-receipt-1"}
	if err := f.server.Events.Ack(t.Context(), p, claimed.Delivery.ID, ack); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Events.Ack(t.Context(), p, claimed.Delivery.ID, ack); err != nil {
		t.Fatal(err)
	}
	ack.Receipt = "different"
	if err := f.server.Events.Ack(t.Context(), p, claimed.Delivery.ID, ack); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("different receipt accepted: %v", err)
	}
	if _, found, err := f.server.Events.Poll(t.Context(), p); err != nil || found {
		t.Fatalf("replayed existing action: %v %v", found, err)
	}
	if err := f.server.Events.ResolveInbox(t.Context(), inbox.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	inbox, err = f.server.Events.InboxEntries(t.Context(), "")
	if err != nil || len(inbox.Items) != 0 {
		t.Fatal("resolved evidence remained in open Inbox")
	}
}

func TestWorkAcceptanceAndQueueAdmissionRollbackTogetherAndRetainTheirOriginalResult(t *testing.T) {
	f := setup(t, "", nil)
	_, p := enroll(t, f, identity.Grant{Key: "automation", Name: "Automation", Kind: "provider", Work: map[string]string{"console:main": "scratch"}})
	req := work.Request{Key: "message-1", Conversation: "console:main", Project: "scratch", Instruction: "Summarize the event", Context: json.RawMessage(`{"body":"/reset is untrusted data"}`)}
	record, err := f.server.Work.Submit(t.Context(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.book.DB().Exec(`CREATE TRIGGER reject_admission BEFORE INSERT ON bindings WHEN new.kind='console-exchange' BEGIN SELECT RAISE(ABORT, 'admission fault'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.Work.Dispatch(t.Context(), record.ID); err == nil {
		t.Fatal("fault was hidden")
	}
	retained, _, err := f.server.Work.Status(t.Context(), p, record.ID)
	if err != nil || retained.State != "pending" || retained.Exchange != "" || len(f.conversation.Queue("console:main")) != 0 || f.runner.calls.Load() != 0 {
		t.Fatalf("partial acceptance escaped transaction: %+v %v", retained, err)
	}
	if _, err := f.book.DB().Exec(`DROP TRIGGER reject_admission`); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Work.Dispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	var result work.Result
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		retained, result, err = f.server.Work.Status(t.Context(), p, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.State == "done" && retained.Outcome != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result.State != "done" || result.Output != "Processed the original event" || retained.Outcome == nil {
		t.Fatalf("no durable final receipt: %+v %+v", retained, result)
	}
	original := retained.Exchange
	if err := f.conversation.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.book.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := setup(t, f.dir, f.runner)
	replay, err := restarted.server.Work.Submit(t.Context(), p, req)
	if err != nil || replay.ID != record.ID || replay.Exchange != original {
		t.Fatalf("retry adopted another exchange: %+v %v", replay, err)
	}
	if err := restarted.server.Work.Dispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	_, after, err := restarted.server.Work.Status(t.Context(), p, record.ID)
	if err != nil || after != result || f.runner.calls.Load() != 1 {
		t.Fatalf("restart repeated or lost execution: %+v %v count=%d", after, err, f.runner.calls.Load())
	}
	req.Instruction = "Different work"
	if _, err := restarted.server.Work.Submit(t.Context(), p, req); !errors.Is(err, ingress.ErrConflict) {
		t.Fatalf("changed retry accepted: %v", err)
	}
}

func TestWorkScopeAndRevocationAreCheckedAtAdmission(t *testing.T) {
	f := setup(t, "", nil)
	g, p := enroll(t, f, identity.Grant{Key: "automation", Name: "Automation", Kind: "provider", Work: map[string]string{"console:main": "scratch"}})
	req := work.Request{Key: "revoked", Conversation: "console:main", Project: "scratch", Instruction: "Summarize"}
	record, err := f.server.Work.Submit(t.Context(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.Identity.Revoke(t.Context(), g.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Work.Dispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	retained, _, err := f.server.Work.Status(t.Context(), identity.Principal{Owner: true}, record.ID)
	if err != nil || retained.State != "failed" || f.runner.calls.Load() != 0 {
		t.Fatalf("revoked grant ran work: %+v %v", retained, err)
	}
	_, p = enroll(t, f, identity.Grant{Key: "second", Name: "Other", Kind: "provider", Work: map[string]string{"console:main": "scratch"}})
	req.Project = "other"
	if _, err := f.server.Work.Submit(t.Context(), p, req); !errors.Is(err, identity.ErrDenied) {
		t.Fatalf("project scope widened: %v", err)
	}
	req.Project = "scratch"
	req.Instruction = "/clear"
	if _, err := f.server.Work.Submit(t.Context(), p, req); !errors.Is(err, work.ErrAdmission) {
		t.Fatalf("provider submitted owner control: %v", err)
	}
}

func TestClientRequiresApprovalAndDoesNotReplayAnUncertainExternalActionAfterRestart(t *testing.T) {
	f := setup(t, "", nil)
	g, p := enroll(t, f, identity.Grant{Key: "mac", Name: "Mac", Kind: "client", Capabilities: []identity.CapabilityGrant{{Capability: "notifications.show", Conversation: "console:main", Approval: true}}})
	input := clientcap.Register{Capabilities: []clientcap.Capability{{Name: "notifications.show", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	registration, err := f.server.Clients.Register(t.Context(), p, input)
	if err != nil {
		t.Fatal(err)
	}
	request := clientcap.Request{Key: "notify", Client: g.Grant.ID, Capability: "notifications.show", Conversation: "console:main", Arguments: json.RawMessage(`{"text":"Hello"}`)}
	call, err := f.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, request)
	if err != nil || call.State != "approval" {
		t.Fatalf("call=%+v %v", call, err)
	}
	if _, found, err := f.server.Clients.Poll(t.Context(), p, registration.Generation); err != nil || found {
		t.Fatal("client received unapproved action")
	}
	if err := f.server.Clients.Approve(t.Context(), call.ID, true); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.server.Clients.Poll(t.Context(), p, registration.Generation)
	if err != nil || !found {
		t.Fatalf("approved claim: %v %v", found, err)
	}
	if err := f.conversation.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.book.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := setup(t, f.dir, f.runner)
	if err := restarted.server.Clients.Recover(t.Context(), call.ID); err != nil {
		t.Fatal(err)
	}
	retained, err := restarted.server.Clients.Status(t.Context(), identity.Principal{Owner: true}, call.ID)
	if err != nil || retained.State != "unknown" || retained.Token != "" {
		t.Fatalf("uncertain result=%+v %v", retained, err)
	}
	if _, found, err := restarted.server.Clients.Poll(t.Context(), p, registration.Generation); err != nil || found {
		t.Fatal("uncertain native action was replayed")
	}
	proof := clientcap.Completion{Token: claimed.Token, State: "done", Receipt: "notification-delivered", Result: json.RawMessage(`{"shown":true}`)}
	if err := restarted.server.Clients.Complete(t.Context(), p, call.ID, proof); err != nil {
		t.Fatal(err)
	}
	retained, err = restarted.server.Clients.Status(t.Context(), identity.Principal{Owner: true}, call.ID)
	if err != nil || retained.State != "done" || retained.Receipt != proof.Receipt {
		t.Fatalf("original proof could not settle uncertainty: %+v %v", retained, err)
	}
	request.Conversation = "console:other"
	if _, err := restarted.server.Clients.Invoke(t.Context(), identity.Principal{Owner: true}, request); err == nil {
		t.Fatal("client capability widened conversation scope")
	}
}

func TestDerivedEventLimitAndInboxPaginationDoNotHideLaterEvents(t *testing.T) {
	f := setup(t, "", nil)
	_, p := enroll(t, f, identity.Grant{Key: "mail", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.*"}})
	for n := range 105 {
		e := publish(t, f, p, fmt.Sprint(n))
		if err := f.server.Events.Route(t.Context(), e.ID); err != nil {
			t.Fatal(err)
		}
	}
	first, err := f.server.Events.InboxEntries(t.Context(), "")
	if err != nil || len(first.Items) != 100 || first.Next == "" {
		t.Fatalf("first page=%d %q %v", len(first.Items), first.Next, err)
	}
	for _, item := range first.Items {
		if err := f.server.Events.ResolveInbox(t.Context(), item.ID); err != nil {
			t.Fatal(err)
		}
	}
	last, err := f.server.Events.InboxEntries(t.Context(), first.Next)
	if err != nil || len(last.Items) != 5 {
		t.Fatalf("later events disappeared: %d %v", len(last.Items), err)
	}
	if _, err := f.server.Events.SaveSubscription(t.Context(), events.Subscription{ID: "loop", Name: "Derived work", Enabled: true, EventType: "mail.*", IncludeDerived: true, Mode: "work", Conversation: "console:main", Project: "scratch", Instruction: "Review"}); err != nil {
		t.Fatal(err)
	}
	var terminal events.Event
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		var err error
		terminal, err = events.AppendTx(tx, events.Event{Source: "server:test", Key: "chain-limit", Type: "mail.received", Payload: json.RawMessage(`{}`), Depth: 8, At: time.Now()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Events.Route(t.Context(), terminal.ID); err != nil {
		t.Fatal(err)
	}
	deliveries, err := f.server.Events.Deliveries(t.Context(), "")
	if err != nil || len(deliveries.Items) != 0 {
		t.Fatal("causal limit admitted another effect")
	}
	inbox, err := f.server.Events.InboxEntries(t.Context(), "")
	if err != nil || len(inbox.Items) != 6 {
		t.Fatalf("causal limit not visible: %+v %v", inbox, err)
	}
	// A persistent first page must not starve the remainder of a worker queue.
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		for n := range 70 {
			if err := tx.PutBinding("test-queue", fmt.Sprintf("%03d", n), fmt.Sprintf("%03d", n)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var scanner durable.Scanner
	seen := map[string]bool{}
	for range 4 {
		ids, err := scanner.Next(t.Context(), f.book, "test-queue", 32)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			seen[id] = true
		}
	}
	if len(seen) != 70 {
		t.Fatalf("worker starved %d entries", 70-len(seen))
	}
}

func TestConnectionTokenCannotBecomeConsoleOwnerAndRevocationClosesItsAPI(t *testing.T) {
	f := setup(t, "", nil)
	g, _ := enroll(t, f, identity.Grant{Key: "mail", Name: "Mail", Kind: "provider", EventTypes: []string{"mail.received"}})
	server, err := httpapi.NewServer(readmodel.New(readmodel.Sources{}), httpapi.ServerConfig{Token: "owner-token"})
	if err != nil {
		t.Fatal(err)
	}
	server.SetAssistant(&serviceapi.Services{Identity: f.server.Identity, Events: f.server.Events, Work: f.server.Work, Clients: f.server.Clients})
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	request := func(path, token, body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	body := `{"key":"mail-1","type":"mail.received","payload":{}}`
	if status := request("/services/events", g.Token, body); status != 200 {
		t.Fatalf("provider status=%d", status)
	}
	if status := request("/console/connections", g.Token, `{}`); status != 401 {
		t.Fatalf("provider acquired owner API: %d", status)
	}
	if status := request("/services/events", "owner-token", body); status != 401 {
		t.Fatalf("owner token passed connection API: %d", status)
	}
	if status := request("/services/events", g.Token, `null`); status != 400 {
		t.Fatalf("null envelope accepted: %d", status)
	}
	if err := f.server.Identity.Revoke(t.Context(), g.Grant.ID); err != nil {
		t.Fatal(err)
	}
	if status := request("/services/events", g.Token, body); status != 401 {
		t.Fatalf("revoked token accepted: %d", status)
	}
}
