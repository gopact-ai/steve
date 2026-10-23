package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

type owedCloseCall struct {
	place   harness.Placement
	id      string
	binding nodewire.SessionBinding
}

// owedCloseNode stands in for the node transport. Each close it is sent is
// bound the way the cluster binds one, from the execution the request
// names, so a test sees which execution the node would be asked to end.
type owedCloseNode struct {
	book  *ledger.Ledger
	mu    sync.Mutex
	calls []owedCloseCall
	err   error
}

func (n *owedCloseNode) CloseSession(ctx context.Context, place harness.Placement, id string) error {
	key, ok := execution.KeyOf(ctx)
	if !ok || key.AttemptID == "" {
		return errors.New("a close owed was sent without the execution it ends")
	}
	record, tracked, err := readSessionBinding(ctx, n.book, key, place, id, "")
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, owedCloseCall{place: place, id: id, binding: sessionBinding(record, tracked)})
	return n.err
}

func (n *owedCloseNode) sent() []owedCloseCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]owedCloseCall(nil), n.calls...)
}

type owedCloseFixture struct {
	book     *ledger.Ledger
	store    *state.Store
	tasks    *task.Store
	attempts *attempt.Service
	node     *owedCloseNode
	closes   *applicationOwedCloses
	tracked  task.Task
	record   attempt.Record
	owed     state.OwedClose
}

// newOwedCloseFixture is a conversation that let go of its session ns_owed
// on node-b while node-b could not be reached: the task that ran there has
// finished, the session is archived and its close is owed.
func newOwedCloseFixture(t *testing.T) *owedCloseFixture {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Goal: "answer", Channel: "console:owed", Member: "mock", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if tracked, err = tasks.Begin(tracked.ID, "mock", "node-b", ""); err != nil {
		t.Fatal(err)
	}
	f := &owedCloseFixture{book: book, store: store, tasks: tasks, attempts: attempt.New(book), tracked: tracked}
	f.record = f.seedAttempt(t, "attempt-owed", "native-owed")
	if tracked, err = tasks.FinishAs(tracked.ID, task.OutcomeOK, task.Tokens{}, 0, ""); err != nil {
		t.Fatal(err)
	}
	f.tracked = tracked
	if err := store.SaveSession(state.Session{ConversationID: "console:owed", AgentID: "mock", HarnessID: "mock", NodeID: "node-b", UpstreamID: "ns_owed", Workspace: "/w"}); err != nil {
		t.Fatal(err)
	}
	f.owed = state.OwedClose{NodeID: "node-b", HarnessID: "mock", UpstreamID: "ns_owed", NativeContext: "native-owed", TaskID: tracked.ID, AttemptID: f.record.ID, OwedAt: "2026-09-30T10:00:00Z"}
	if err := store.ArchiveSessionOwingClose("console:owed", "mock", f.owed.OwedAt, f.owed); err != nil {
		t.Fatal(err)
	}
	f.node = &owedCloseNode{book: book}
	f.closes = newApplicationOwedCloses(store, f.attempts, tasks, f.node)
	return f
}

// seedAttempt commits an execution of the fixture's task in ns_owed.
func (f *owedCloseFixture) seedAttempt(t *testing.T, id, native string) attempt.Record {
	t.Helper()
	settled := true
	r := attempt.Record{
		Spec: attempt.Spec{
			ID: id, TaskID: f.tracked.ID, TurnID: "turn-" + id, Kind: attempt.KindChat, Project: "p",
			Node: "node-b", Harness: "mock", Agent: "mock",
			Execution: &task.ExecutionToken{TaskID: f.tracked.ID, Epoch: f.tracked.ExecutionEpoch},
		},
		State: attempt.Bound, Session: "ns_owed", NativeContext: native, SessionSettled: &settled,
		StartedAt: time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC),
	}
	if _, err := f.book.Begin(t.Context(), r.ID, "attempt", string(r.State), "", r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *owedCloseFixture) reconcile(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_ = f.closes.Reconcile(ctx)
}

// The close is sent again on every pass until the node takes it, then
// forgotten; each one is bound to the execution that last ran there.
func TestOwedCloseIsSentUntilTheNodeTakesIt(t *testing.T) {
	f := newOwedCloseFixture(t)
	want := owedCloseCall{place: harness.Placement{Node: "node-b", Harness: "mock"}, id: "ns_owed", binding: sessionBinding(f.record, f.tracked)}
	for round, failure := range []error{
		&nodewire.SessionNotDispatched{Cause: errors.New("node-b is offline")},
		&nodewire.SessionNotDispatched{Cause: errors.New("node-b is offline")},
		&node.SessionError{Code: "uncertain", Message: "native process stop is not confirmed"},
	} {
		f.node.err = failure
		f.reconcile(t)
		if sent := f.node.sent(); len(sent) != round+1 || !reflect.DeepEqual(sent[round], want) {
			t.Fatalf("round %d: closes sent %+v, want %d of %+v", round, sent, round+1, want)
		}
		if got := f.store.OwedCloses(); !reflect.DeepEqual(got, []state.OwedClose{f.owed}) {
			t.Fatalf("round %d: a close the node did not take (%v) is owed %+v, want %+v", round, failure, got, []state.OwedClose{f.owed})
		}
	}
	f.node.err = nil
	f.reconcile(t)
	if sent := f.node.sent(); len(sent) != 4 || !reflect.DeepEqual(sent[3], want) {
		t.Fatalf("closes sent %+v once the node is back", sent)
	}
	if got := f.store.OwedCloses(); len(got) != 0 {
		t.Fatalf("a close the node took is still owed: %+v", got)
	}
	reopened, err := state.OpenLedger(f.book)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.OwedCloses(); len(got) != 0 {
		t.Fatalf("a close the node took is owed again after a restart: %+v", got)
	}
	f.reconcile(t)
	if sent := f.node.sent(); len(sent) != 4 {
		t.Fatalf("a close already taken was sent again: %+v", sent)
	}
}

// A coordinator that takes over, or restarts, finds the close still owed
// and sends it.
func TestOwedCloseSurvivesARestart(t *testing.T) {
	f := newOwedCloseFixture(t)
	store, err := state.OpenLedger(f.book)
	if err != nil {
		t.Fatal(err)
	}
	next := newApplicationOwedCloses(store, attempt.New(f.book), f.tasks, f.node)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_ = next.Reconcile(ctx)
	if sent := f.node.sent(); len(sent) != 1 || sent[0].id != "ns_owed" || sent[0].binding.AttemptID != f.record.ID {
		t.Fatalf("the restarted coordinator sent %+v", sent)
	}
	if got := store.OwedCloses(); len(got) != 0 {
		t.Fatalf("still owed after the restarted coordinator closed it: %+v", got)
	}
}

// A close is only ever sent to the execution it was owed for. Once that is
// no longer what the session holds, or no longer on record, the close is
// not sent, and the debt goes: nothing could ever send it rightly again.
func TestOwedCloseIsNotSentToAnotherExecution(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *owedCloseFixture){
		"a later execution took the session up": func(t *testing.T, f *owedCloseFixture) {
			f.seedAttempt(t, "attempt-later", "native-owed")
		},
		"the session holds another native context": func(t *testing.T, f *owedCloseFixture) {
			f.owed.NativeContext = "native-before"
			f.replaceOwed(t)
		},
		"the execution is another task's": func(t *testing.T, f *owedCloseFixture) {
			other, err := f.tasks.Create(task.Task{Goal: "another", Channel: "console:other", Member: "mock", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			f.owed.TaskID = other.ID
			f.replaceOwed(t)
		},
		"the task is gone": func(t *testing.T, f *owedCloseFixture) {
			if _, err := f.tasks.DeleteChannel(t.Context(), f.tracked.Channel, nil); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOwedCloseFixture(t)
			change(t, f)
			f.reconcile(t)
			if sent := f.node.sent(); len(sent) != 0 {
				t.Fatalf("closed another execution's session: %+v", sent)
			}
			if got := f.store.OwedCloses(); len(got) != 0 {
				t.Fatalf("a close that can never be sent rightly is still owed: %+v", got)
			}
		})
	}
}

// replaceOwed owes the fixture's session again with f.owed as it now reads.
func (f *owedCloseFixture) replaceOwed(t *testing.T) {
	t.Helper()
	if _, err := f.store.RestoreSession("console:owed", "mock", 1); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ArchiveSessionOwingClose("console:owed", "mock", f.owed.OwedAt, f.owed); err != nil {
		t.Fatal(err)
	}
	if got := f.store.OwedCloses(); !reflect.DeepEqual(got, []state.OwedClose{f.owed}) {
		t.Fatalf("owed %+v, want %+v", got, []state.OwedClose{f.owed})
	}
}

// History that cannot be read says nothing about the execution; the close
// stays owed rather than being judged against a guess.
func TestOwedCloseStaysOwedWhenHistoryCannotBeRead(t *testing.T) {
	f := newOwedCloseFixture(t)
	unreadable, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unreadable.Close(); err != nil {
		t.Fatal(err)
	}
	f.closes = newApplicationOwedCloses(f.store, attempt.New(unreadable), f.tasks, f.node)
	f.reconcile(t)
	if sent := f.node.sent(); len(sent) != 0 {
		t.Fatalf("sent %+v without reading the execution", sent)
	}
	if got := f.store.OwedCloses(); !reflect.DeepEqual(got, []state.OwedClose{f.owed}) {
		t.Fatalf("owed %+v after an unreadable pass, want %+v", got, []state.OwedClose{f.owed})
	}
}

func TestOwedCloseKeepsOnlyRefusalsThatCanChange(t *testing.T) {
	for _, code := range []string{"conflict", "absent", "forbidden", "unavailable", "busy", "uncertain"} {
		t.Run(code, func(t *testing.T) {
			f := newOwedCloseFixture(t)
			f.node.err = fmt.Errorf("close session: %w", &node.SessionError{Code: code, Message: "node refused"})
			f.reconcile(t)
			kept := code != "conflict" && code != "absent"
			if got := len(f.store.OwedCloses()); (got == 1) != kept {
				t.Fatalf("refusal %s leaves %d closes owed, want kept=%v", code, got, kept)
			}
		})
	}
}
