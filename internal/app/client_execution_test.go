package app

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestClientDispatchRequiresTheOriginalLiveAttemptAndProcessGeneration(t *testing.T) {
	book, tasks := applicationGrantBook(t)
	root, err := tasks.Create(task.Task{Channel: "console:main", Transport: "console", ChatType: "p2p", Requester: "owner", Member: "agent", ProjectID: "scratch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(root.ID, "agent", "hub", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	r, err := attempts.Open(t.Context(), attempt.Spec{ID: "original-client-attempt", TaskID: root.ID, TurnID: "turn", Execution: &token, Kind: attempt.KindChat, By: "owner", Project: "scratch", Node: "hub", Agent: "agent", Harness: "mock", Scope: attempt.ScopeNone, Workspace: project.Workspace{ID: "work", Project: "scratch", Node: "hub", Kind: project.KindWorktree, Path: filepath.Join(t.TempDir(), "work")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Advance(t.Context(), r.ID, attempt.Prepared, "test", nil); err != nil {
		t.Fatal(err)
	}
	r, err = attempts.RecordSession(t.Context(), r.ID, "test", "ns_client-original")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Advance(t.Context(), r.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	identities := &identity.Service{Book: book, OwnerID: "owner"}
	client, err := identities.Enroll(t.Context(), identity.Grant{Key: "mac", Name: "Mac", Kind: "client", Capabilities: []identity.CapabilityGrant{{Capability: "notifications.show", Conversation: "console:main"}}})
	if err != nil {
		t.Fatal(err)
	}
	broker := &clientcap.Broker{Book: book, Identity: identities, CheckExecution: func(tx *ledger.Tx, c clientcap.Invocation) error { return checkClientExecution(tx, "owner", c) }}
	p := identity.Principal{GrantID: client.Grant.ID}
	reg, err := broker.Register(t.Context(), p, clientcap.Register{Capabilities: []clientcap.Capability{{Name: "notifications.show", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	execution := clientcap.Execution{Task: root.ID, Attempt: r.ID, Epoch: token.Epoch, Generation: attempt.SessionExecutionEpoch(r)}
	request := clientcap.Request{Key: "native-notification", Client: client.Grant.ID, Capability: "notifications.show", Conversation: "console:main", Arguments: json.RawMessage(`{}`)}
	forged := execution
	forged.Generation++
	if _, err := broker.Submit(t.Context(), identity.Principal{Owner: true}, request, &forged, nil); !errors.Is(err, identity.ErrDenied) {
		t.Fatalf("wrong process generation admitted: %v", err)
	}
	accepted, err := broker.Submit(t.Context(), identity.Principal{Owner: true}, request, &execution, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempts.MarkSessionSettled(t.Context(), r.ID, "original process ended"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := broker.Poll(t.Context(), p, reg.Generation); err != nil || found {
		t.Fatalf("ended execution dispatched native action: %v %v", found, err)
	}
	final, err := broker.Status(t.Context(), identity.Principal{Owner: true}, accepted.ID)
	if err != nil || final.State != clientcap.StateCancelled {
		t.Fatalf("lost original execution fence: %+v %v", final, err)
	}
}
