package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/procgroup"
	"time"
)

// failedSession is a running session whose journal has failed and whose
// record names no process group: nothing can prove its agent ended.
func failedSession(t *testing.T) (*SessionService, *ownedSession, error) {
	t.Helper()
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.sessions.Close)
	req := nodeSessionRequest("prompt")
	req.ID = "ns_" + sessionHash("failed-journal")
	one := &ownedSession{service: s.sessions, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
	record := sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, State: nodewire.SessionState{ID: req.ID, Binding: req.Binding, Harness: "mock", State: "running", InputAccepted: 1}, CurrentCommand: "input-1", Commands: map[string]nodewire.SessionCommand{"input-1": {ID: "input-1", InputSequence: 1, State: "running", DispatchState: "dispatched"}}, CommandHashes: map[string]string{"input-1": "hash"}}
	if err := one.commitLocked(record); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("node session persistence failed: question journal is full")
	one.failure = failure
	s.sessions.sessions[req.ID] = one
	return s.sessions, one, failure
}

func TestFailedSessionStopWithoutProofStaysIsolated(t *testing.T) {
	s, one, failure := failedSession(t)
	req := nodeSessionRequest(nodewire.SessionActionKill)
	req.ID, req.CommandID = one.record.State.ID, "input-1"
	if _, err := s.Do(t.Context(), "cluster-1", req); !errors.Is(err, failure) {
		t.Fatalf("unprovable stop of a failed session = %v", err)
	}
	record, _, err := s.readRecord(req.ID)
	if err != nil || record.State.ProcessStopped {
		t.Fatalf("unprovable stop was recorded: %+v %v", record.State, err)
	}
	if s.sessions[req.ID] != one {
		t.Fatal("unprovable stop released the failed owner")
	}
	// Observation still reads the durable receipt instead of the failure.
	req.Action = nodewire.SessionActionAttach
	state, err := s.Do(t.Context(), "cluster-1", req)
	if err != nil || state.ProcessStopped || state.Command == nil || state.Command.ID != "input-1" {
		t.Fatalf("failed session observation = %+v %v", state, err)
	}
	// Anything else still answers the failure.
	req.Action, req.InputSequence, req.Text = nodewire.SessionActionPrompt, 2, "more"
	if _, err := s.Do(t.Context(), "cluster-1", req); !errors.Is(err, failure) {
		t.Fatalf("failed session admitted input: %v", err)
	}
}

func TestFailedSessionStopRefusesAnotherExecution(t *testing.T) {
	s, one, _ := failedSession(t)
	req := nodeSessionRequest(nodewire.SessionActionKill)
	req.ID = one.record.State.ID
	req.Binding.AttemptID = "another-attempt"
	if _, err := s.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("another execution stopped a failed session")
	}
	// The owner check holds even for a request the coordinator authorized.
	if _, err := s.doFailed(t.Context(), one, one.failure, req); err == nil || !strings.Contains(err.Error(), "another execution") {
		t.Fatalf("failed owner stopped for another execution: %v", err)
	}
	if s.sessions[one.record.State.ID] != one {
		t.Fatal("refused stop released the failed owner")
	}
}

func TestFailedSessionStopProvedByRecordedGroup(t *testing.T) {
	s, one, _ := failedSession(t)
	place, err := procgroup.Here()
	if err != nil {
		t.Skip("process groups are not tracked here")
	}
	s.place, s.placeKnown = place, true
	settled := 0
	s.settle = func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error { settled++; return nil }
	// The group is recorded before the journal fails.
	one.failure = nil
	next := one.copyLocked()
	next.Process = sessionProcess{Identity: procgroup.Identity{Group: 4242, Leader: 4242, Start: 1, Mark: "m"}, Place: place}
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("node session persistence failed")
	one.failure = failure
	req := nodeSessionRequest(nodewire.SessionActionCancel)
	req.ID, req.CommandID = one.record.State.ID, "input-1"
	state, err := s.Do(t.Context(), "cluster-1", req)
	if err != nil || !state.ProcessStopped || settled != 1 {
		t.Fatalf("recorded group did not prove the stop: settled=%d %+v %v", settled, state, err)
	}
	if state.Command == nil || !state.Command.ProcessStopped || state.Command.State.Active() {
		t.Fatalf("stopped command still active: %+v", state.Command)
	}
	if _, live := s.sessions[req.ID]; live {
		t.Fatal("stopped failed owner was not released")
	}
}

// Questions of earlier commands count toward the journal's bound. Asking
// past it is refused before the write, so the session is not latched failed.
func TestNodeQuestionBoundCountsTheWholeJournal(t *testing.T) {
	one := progressSession(t)
	store, err := one.service.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET question_count=? WHERE id=?`, sessionQuestionLimit, one.record.State.ID); err != nil {
		t.Fatal(err)
	}
	_, err = one.waitQuestion(t.Context(), nodewire.SessionQuestion{CommandID: "input"})
	if err == nil || !strings.Contains(err.Error(), "retention limit") {
		t.Fatalf("question past the journal bound = %v", err)
	}
	one.mu.Lock()
	failure := one.failure
	one.mu.Unlock()
	if failure != nil {
		t.Fatalf("question bound latched the session failed: %v", failure)
	}
}
