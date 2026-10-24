package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/procgroup"
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
	settled := 0
	recordGroup(t, s, one, func() error { settled++; return nil })
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

// recordGroup records a process group on a failed session's record and
// makes the node prove groups with settle.
func recordGroup(t *testing.T, s *SessionService, one *ownedSession, settle func() error) {
	t.Helper()
	place, err := procgroup.Here()
	if err != nil {
		t.Skip("process groups are not tracked here")
	}
	s.place, s.placeKnown = place, true
	s.settle = func(procgroup.Identity, procgroup.Place, procgroup.Place, time.Duration) error { return settle() }
	failure := one.failure
	one.failure = nil
	next := one.copyLocked()
	next.Process = sessionProcess{Identity: procgroup.Identity{Group: 4242, Leader: 4242, Start: 1, Mark: "m"}, Place: place}
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	one.failure = failure
}

// Once the stop is durable the owner is released even when the journal
// refuses the state change after it, as a journal at its byte bound does:
// the stop's own small write fits, the command's error text does not. Every
// later stop is then answered from the durable record, never wedged.
func TestFailedSessionStopSurvivesByteBoundAfterRecordedStop(t *testing.T) {
	s, req := byteBoundStop(t)
	requireAnsweredStopped(t, s, req)
}

// A node restarting with such a record loads it without the interrupted
// state the journal still refuses, and keeps answering its stop from the
// record; the other sessions load as usual.
func TestFailedSessionStopSurvivesRestartAtByteBound(t *testing.T) {
	s, req := byteBoundStop(t)
	server := s.server
	s.Close()
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatalf("node session service failed to restart: %v", err)
	}
	restarted := server.sessions
	t.Cleanup(restarted.Close)
	durable, _, err := restarted.readRecord(req.ID)
	if err != nil || !durable.State.ProcessStopped || durable.State.State != "running" {
		t.Fatalf("fixture did not refuse the state change at restart: %+v %v", durable.State, err)
	}
	requireAnsweredStopped(t, restarted, req)
}

// byteBoundStop cancels a failed session whose journal has room for its
// recorded stop and nothing more, so the stop is durable and the state
// change after it is refused, as at the journal's byte bound.
func byteBoundStop(t *testing.T) (*SessionService, nodewire.SessionRequest) {
	t.Helper()
	s, one, _ := failedSession(t)
	recordGroup(t, s, one, func() error { return nil })
	// Leave room for exactly the recorded stop, computed as
	// commitRecordedStop writes it, and a few bytes more.
	before := one.copyLocked()
	stopped := one.copyLocked()
	stopped.State.Sequence++
	stopped.State.ProcessStopped = true
	a, _ := sessionRecordJSON(sessionHeader(before))
	b, _ := sessionRecordJSON(sessionHeader(stopped))
	growth := len(b) - len(a)
	command := before.Commands["input-1"]
	a, _ = sessionRecordJSON(command)
	command.ProcessStopped = true
	b, _ = sessionRecordJSON(command)
	growth += len(b) - len(a)
	store, err := s.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET retained_bytes=? WHERE id=?`, nodewire.NodeSessionMaxBytes-growth-8, one.record.State.ID); err != nil {
		t.Fatal(err)
	}
	req := nodeSessionRequest(nodewire.SessionActionCancel)
	req.ID, req.CommandID = one.record.State.ID, "input-1"
	state, err := s.Do(t.Context(), "cluster-1", req)
	if err != nil || !state.ProcessStopped {
		t.Fatalf("stop behind a byte-bound journal = %+v %v", state, err)
	}
	durable, _, err := s.readRecord(req.ID)
	if err != nil || !durable.State.ProcessStopped || durable.State.State != "running" {
		t.Fatalf("fixture did not refuse the state change after the stop: %+v %v", durable.State, err)
	}
	if _, live := s.sessions[req.ID]; live {
		t.Fatal("owner with a durable stop was not released")
	}
	return s, req
}

func requireAnsweredStopped(t *testing.T, s *SessionService, req nodewire.SessionRequest) {
	t.Helper()
	for _, action := range []nodewire.SessionAction{nodewire.SessionActionKill, nodewire.SessionActionCancel, nodewire.SessionActionAttach} {
		req.Action = action
		state, err := s.Do(t.Context(), "cluster-1", req)
		if err != nil || !state.ProcessStopped || state.Command == nil || state.Command.ID != "input-1" {
			t.Fatalf("%s after a durable stop = %+v %v", action, state, err)
		}
	}
}

// failedHost is a host whose agent is running; stopped says whether the
// host can see that agent's process exit once it is killed.
func failedHost(t *testing.T, one *ownedSession, stopped bool) {
	t.Helper()
	broker, _ := permission.New("read")
	local := acphost.LocalTransport{Command: buildMockAgent(t), ProcessDir: t.TempDir()}
	var transport acphost.Transport = local
	if !stopped {
		// The host can neither kill this agent nor see it exit; the test
		// ends it itself.
		unproven := &unprovenTransport{inner: local}
		t.Cleanup(func() {
			if unproven.started != nil {
				unproven.started.Kill()
			}
		})
		transport = unproven
	}
	host := acphost.New(acphost.Config{NoRestart: true, Permission: broker, Transport: transport})
	t.Cleanup(host.Close)
	if _, _, err := host.OpenSession(t.Context(), "", acphost.SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	one.mu.Lock()
	one.host = host
	one.mu.Unlock()
}

// unprovenTransport starts agents whose exit the host can never see.
type unprovenTransport struct {
	inner   acphost.Transport
	started acphost.Process
}

func (tr *unprovenTransport) Name() string { return "unproven-mock" }
func (tr *unprovenTransport) Start(ctx context.Context) (acphost.Process, error) {
	p, err := tr.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	tr.started = p
	return unprovenProcess{p}, nil
}

type unprovenProcess struct{ acphost.Process }

func (unprovenProcess) Stopped() bool { return false }

// With no group recorded, the host that started the agent proves its exit
// once it has seen every process it started end. A host that can't see
// that, or a group check that found members, proves nothing.
func TestFailedSessionStopProvedByHostWithoutRecordedGroup(t *testing.T) {
	for _, mode := range []string{"host-proves", "host-unproven", "group-running"} {
		t.Run(mode, func(t *testing.T) {
			s, one, failure := failedSession(t)
			if mode == "group-running" {
				recordGroup(t, s, one, func() error { return procgroup.ErrRunning })
			}
			failedHost(t, one, mode != "host-unproven")
			req := nodeSessionRequest(nodewire.SessionActionKill)
			req.ID, req.CommandID = one.record.State.ID, "input-1"
			state, err := s.Do(t.Context(), "cluster-1", req)
			durable, _, readErr := s.readRecord(req.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "host-proves" {
				if err != nil || !state.ProcessStopped || !durable.State.ProcessStopped || durable.Process.Identity != (procgroup.Identity{}) {
					t.Fatalf("host did not prove the stop: %+v %v", state, err)
				}
				if _, live := s.sessions[req.ID]; live {
					t.Fatal("proved owner was not released")
				}
				return
			}
			if !errors.Is(err, failure) || durable.State.ProcessStopped {
				t.Fatalf("unproved stop was confirmed: stopped=%v err=%v", durable.State.ProcessStopped, err)
			}
			if s.sessions[req.ID] != one {
				t.Fatal("unproved stop released the failed owner")
			}
		})
	}
}

// A stop whose identity the record doesn't hold is refused before the
// process is touched: retiring a failed owner can't be undone.
func TestFailedSessionStopRefusesWrongIdentityBeforeEnding(t *testing.T) {
	for _, mode := range []string{"close-harness", "close-input", "close-command", "kill-other-command", "cancel-expired-receipt"} {
		t.Run(mode, func(t *testing.T) {
			s, one, _ := failedSession(t)
			settled := 0
			recordGroup(t, s, one, func() error { settled++; return nil })
			req := nodeSessionRequest(nodewire.SessionActionClose)
			req.ID, req.CommandID = one.record.State.ID, "input-1"
			switch mode {
			case "close-harness":
				req.Harness = "another"
			case "close-input":
				req.InputSequence = 7
			case "close-command":
				req.CommandID = "never-sent"
			case "kill-other-command":
				req.Action, req.CommandID = nodewire.SessionActionKill, "another-input"
			case "cancel-expired-receipt":
				req.Action, req.CommandID, req.InputSequence = nodewire.SessionActionCancel, "pruned-input", 1
			}
			if _, err := s.Do(t.Context(), "cluster-1", req); err == nil {
				t.Fatal("stop with a wrong identity was accepted")
			}
			durable, _, err := s.readRecord(req.ID)
			if err != nil || durable.State.ProcessStopped || settled != 0 {
				t.Fatalf("refused stop ended the process: stopped=%v settled=%d %v", durable.State.ProcessStopped, settled, err)
			}
			if s.sessions[req.ID] != one {
				t.Fatal("refused stop released the failed owner")
			}
		})
	}
}

// A workspace callback still running keeps the stop unproved even when the
// recorded group is proved gone; once it drains the same stop is recorded.
func TestFailedSessionStopWaitsForWorkspaceCallbacks(t *testing.T) {
	s, one, failure := failedSession(t)
	recordGroup(t, s, one, func() error { return nil })
	failedHost(t, one, true)
	drained := false
	s.hostDrained = func(*acphost.Host) bool { return drained }
	req := nodeSessionRequest(nodewire.SessionActionKill)
	req.ID, req.CommandID = one.record.State.ID, "input-1"
	_, err := s.Do(t.Context(), "cluster-1", req)
	var refusal *SessionError
	if !errors.Is(err, failure) || !errors.As(err, &refusal) || refusal.Code != "stop_unproven" {
		t.Fatalf("stop with a running callback = %v", err)
	}
	durable, _, err := s.readRecord(req.ID)
	if err != nil || durable.State.ProcessStopped || s.sessions[req.ID] != one {
		t.Fatalf("stop with a running callback was recorded: %+v %v", durable.State, err)
	}
	drained = true
	if state, err := s.Do(t.Context(), "cluster-1", req); err != nil || !state.ProcessStopped {
		t.Fatalf("stop after callbacks drained = %+v %v", state, err)
	}
}

// failedOpen is a failed owner whose open never answered: the hub knows no
// session ID and recovers it by its open command.
func failedOpen(t *testing.T) (*SessionService, *ownedSession, nodewire.SessionRequest) {
	t.Helper()
	s, one, _ := failedSession(t)
	req := nodeSessionRequest(nodewire.SessionActionKill)
	req.CommandID, req.Harness = "input-1/open", "mock"
	id := nodewire.SessionOpenID(req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness)
	failure := one.failure
	delete(s.sessions, one.record.State.ID)
	open := &ownedSession{service: s, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
	record := sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, OpenID: req.CommandID, State: nodewire.SessionState{ID: id, Binding: req.Binding, Harness: "mock", State: nodewire.SessionOpening}, Commands: map[string]nodewire.SessionCommand{}, CommandHashes: map[string]string{}}
	if err := open.commitLocked(record); err != nil {
		t.Fatal(err)
	}
	open.failure = failure
	s.sessions[id] = open
	return s, open, req
}

// Open recovery reaches a failed owner too: kill and cancel-open end it and
// answer the original open receipt, closed, even when the journal refuses
// the Closed state; a retry is answered from the record.
func TestFailedOpenRecoveryStopsFailedOwner(t *testing.T) {
	for _, mode := range []string{"kill", "cancel-open", "closed-refused"} {
		t.Run(mode, func(t *testing.T) {
			s, one, req := failedOpen(t)
			recordGroup(t, s, one, func() error { return nil })
			if mode == "cancel-open" {
				req.Action = nodewire.SessionActionCancelOpen
			}
			if mode == "closed-refused" {
				store, err := s.recordsStore()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.Exec(`CREATE TRIGGER refuse_closed BEFORE UPDATE OF header ON sessions WHEN json_extract(NEW.header,'$.state.state') = 'closed' BEGIN SELECT RAISE(ABORT, 'CHECK constraint failed: retained_bytes'); END`); err != nil {
					t.Fatal(err)
				}
			}
			id := one.record.State.ID
			for attempt := range 2 {
				state, err := s.Do(t.Context(), "cluster-1", req)
				if err != nil || state.OpenReceipt == nil || state.OpenReceipt.Action != req.Action || state.OpenReceipt.CommandID != req.CommandID ||
					state.ID != id || !state.ProcessStopped || state.State != nodewire.SessionClosed {
					t.Fatalf("open recovery %d of a failed owner = %+v %v", attempt, state, err)
				}
				if _, live := s.sessions[id]; live {
					t.Fatal("failed open owner was not released")
				}
			}
			if mode == "closed-refused" {
				durable, _, err := s.readRecord(id)
				if err != nil || durable.State.State == nodewire.SessionClosed {
					t.Fatalf("fixture did not refuse the Closed state: %+v %v", durable.State, err)
				}
			}
		})
	}
}

// A plugin use whose settlement fails keeps the owner, so a later stop
// settles it; releasing the owner first would strand the use until restart.
func TestFailedSessionStopRetriesPluginSettlement(t *testing.T) {
	s, one, failure := failedSession(t)
	recordGroup(t, s, one, func() error { return nil })
	selection := nodeRuntimeFixture(t, s.server, "")
	runtime, err := s.server.pluginRuntimePool().Prepare(t.Context(), "native", selection, harness.Config{Command: buildMockAgent(t), Permission: "read"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.server.closePluginRuntimes)
	id := one.record.State.ID
	if err := s.server.pluginStore().BeginRuntimeUse(t.Context(), runtime.Ref, "session/"+id, "session"); err != nil {
		t.Fatal(err)
	}
	one.failure = nil
	next := one.copyLocked()
	next.State.Plugin = runtime.Ref.Clone()
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	one.failure = failure
	uses := filepath.Join(s.server.pluginStore().Dir, "runtimes", runtime.Ref.ID, "uses")
	if err := os.Chmod(uses, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(uses, 0700) })
	req := nodeSessionRequest(nodewire.SessionActionKill)
	req.ID, req.CommandID = id, "input-1"
	if _, err := s.Do(t.Context(), "cluster-1", req); !errors.Is(err, failure) {
		t.Fatalf("stop with an unsettled plugin use = %v", err)
	}
	if s.sessions[id] != one {
		t.Fatal("owner released with its plugin use unsettled")
	}
	if err := os.Chmod(uses, 0700); err != nil {
		t.Fatal(err)
	}
	if state, err := s.Do(t.Context(), "cluster-1", req); err != nil || !state.ProcessStopped {
		t.Fatalf("retried stop = %+v %v", state, err)
	}
	if _, live := s.sessions[id]; live {
		t.Fatal("settled owner was not released")
	}
	infos, err := s.server.pluginStore().RuntimeInfos()
	if err != nil || len(infos) != 1 || len(infos[0].Uses) != 1 || !infos[0].Uses[0].Stopped {
		t.Fatalf("plugin use left active: %+v %v", infos, err)
	}
}
