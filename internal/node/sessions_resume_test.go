package node

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
)

func resumedFixture(t *testing.T, command string) (ServerConfig, nodewire.SessionRequest, sessionRecord) {
	t.Helper()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID = "mock", t.TempDir(), "next/open"
	req.ID = "ns_" + sessionHash("retained-history")
	record := sessionRecord{Format: 1, ClusterID: "cluster-1", Authority: req.Authority, ConfigHash: sessionConfigHash(req), UpstreamID: "native-history-42", OpenID: "original/open", State: nodewire.SessionState{ID: req.ID, Binding: req.Binding, Harness: req.Harness, State: nodewire.SessionInterrupted, ProcessStopped: true, InputAccepted: 1}, Commands: map[string]nodewire.SessionCommand{"old-input": {ID: "old-input", InputSequence: 1, State: nodewire.SessionCommandCompleted, Settled: true, ProcessStopped: true, Output: "original answer"}}, CommandHashes: map[string]string{"old-input": "original-hash"}, CurrentCommand: "old-input"}
	req.Binding.TaskID = "task-2"
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: command}}, SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}}
	return cfg, req, record
}

func saveResumeFixture(t *testing.T, cfg ServerConfig, record sessionRecord) sessionRecord {
	t.Helper()
	store, err := openSessionRecords(filepath.Join(cfg.StateDir, "node-sessions", "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	return saveSessionRecordsFixture(t, store, record)
}

func saveSessionRecordsFixture(t *testing.T, store *sessionRecords, record sessionRecord) sessionRecord {
	t.Helper()
	before, found, err := store.read(record.State.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		before = sessionRecord{}
	}
	record.State.Sequence = before.State.Sequence + 1
	for _, command := range record.Commands {
		record.State.InputAccepted = max(record.State.InputAccepted, command.InputSequence)
	}
	for i := range record.State.Questions {
		q := &record.State.Questions[i]
		if q.ID == "" {
			q.ID = fmt.Sprintf("fixture-question-%d", i)
		}
		if q.CommandID == "" {
			q.CommandID = record.CurrentCommand
		}
	}
	if err := store.save(before, record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestSettledNativeContextResumesOnceAndPreservesOriginalReceipts(t *testing.T) {
	cfg, req, old := resumedFixture(t, buildMockAgent(t))
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	var wg sync.WaitGroup
	states := make([]nodewire.SessionState, 2)
	errs := make([]error, 2)
	for i := range states {
		wg.Add(1)
		go func() { defer wg.Done(); states[i], errs[i] = s.sessions.Do(t.Context(), "cluster-1", req) }()
	}
	wg.Wait()
	for i, state := range states {
		if errs[i] != nil || state.ID == old.State.ID || state.ID != states[0].ID || state.Binding != req.Binding || state.InputAccepted != 0 {
			t.Fatalf("resume did not reserve one fresh execution: %+v %v", state, errs[i])
		}
	}
	if len(s.sessions.sessions) != 1 {
		t.Fatal("duplicate resume started more than one process")
	}
	current, _, err := s.sessions.readRecord(states[0].ID)
	if err != nil || current.UpstreamID != old.UpstreamID || current.ResumedFrom != old.State.ID {
		t.Fatalf("resume replaced native context: %+v %v", current, err)
	}
	archived, _, _ := s.sessions.readRecord(old.State.ID)
	if !reflect.DeepEqual(archived.Commands, old.Commands) || !reflect.DeepEqual(archived.CommandHashes, old.CommandHashes) || archived.State.Binding != old.State.Binding || archived.ResumeTarget != current.State.ID {
		t.Fatal("resume rewrote the original execution or lost its exclusive claim")
	}
	other := req
	other.Binding.TaskID, other.CommandID = "task-3", "competing/open"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", other); err == nil {
		t.Fatal("another execution branched a live native context")
	}
	inspect := req
	inspect.ID, inspect.Action = "", nodewire.SessionActionInspectOpen
	proof, err := s.sessions.Do(t.Context(), "cluster-1", inspect)
	if err != nil || proof.ID != current.State.ID || proof.OpenReceipt == nil || proof.InputAccepted != 0 {
		t.Fatalf("lost resume response cannot be reconciled by original open: %+v %v", proof, err)
	}
	input := req
	input.ID, input.Action, input.CommandID, input.InputSequence, input.Text = current.State.ID, nodewire.SessionActionPrompt, "new-input", 1, "a fresh continuation"
	state, err := s.sessions.Do(t.Context(), "cluster-1", input)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if state.Command != nil && state.Command.Settled {
			break
		}
		poll := input
		poll.Action, poll.After, poll.WaitMS = nodewire.SessionActionPoll, state.Sequence, 50
		state, err = s.sessions.Do(t.Context(), "cluster-1", poll)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.Command == nil || !state.Command.Settled || !strings.Contains(state.Command.Output, input.Text) || state.InputAccepted != 1 {
		t.Fatalf("new input did not settle: %+v", state)
	}
	if repeated, err := s.sessions.Do(t.Context(), "cluster-1", input); err != nil || repeated.InputAccepted != 1 {
		t.Fatal("fresh input receipt was replayed")
	}
	warm := req
	warm.ID, warm.Binding.TaskID, warm.CommandID = current.State.ID, "warm-task", "warm/open"
	if state, err := s.sessions.Do(t.Context(), "cluster-1", warm); err != nil || state.ID != current.State.ID || state.Binding != warm.Binding || state.ContextID != old.State.ID {
		t.Fatalf("live continuation entered cold resume: %+v %v", state, err)
	}
	s.sessions.Close()
	restarted := NewServer(cfg)
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	// A coordinator that lost a successful open response may still name the
	// oldest managed session. Follow only the settled, stopped handoff chain.
	third, err := restarted.sessions.Do(t.Context(), "cluster-1", other)
	if err != nil || third.ID == current.State.ID || third.InputAccepted != 0 {
		t.Fatalf("second restart lost context chain: %+v %v", third, err)
	}
	last, _, _ := restarted.sessions.readRecord(third.ID)
	if last.UpstreamID != old.UpstreamID || last.ResumedFrom != current.State.ID {
		t.Fatal("context chain started a different native conversation")
	}
}

func TestNativeResumeRejectsUncertainSourcesAndChangedAuthorityBeforeStart(t *testing.T) {
	for _, mode := range []string{"process", "receipt", "uncertain-input", "unknown-question", "missing-answer", "native-id", "project", "conversation", "workdir", "policy", "mcp-binding", "coordinator", "start", "cancelled-open"} {
		t.Run(mode, func(t *testing.T) {
			cfg, req, record := resumedFixture(t, "/must-not-start")
			switch mode {
			case "process":
				record.State.ProcessStopped = false
			case "receipt":
				c := record.Commands["old-input"]
				c.Settled = false
				record.Commands["old-input"] = c
			case "uncertain-input":
				c := record.Commands["old-input"]
				c.State = nodewire.SessionCommandUncertain
				record.Commands["old-input"] = c
			case "unknown-question":
				record.State.Questions = []nodewire.SessionQuestion{{State: "unknown"}}
			case "missing-answer":
				record.State.Questions = []nodewire.SessionQuestion{{State: "answered"}}
			case "native-id":
				record.UpstreamID = ""
			case "project":
				req.Binding.ProjectID = "different"
			case "conversation":
				req.Binding.SessionID = "different"
			case "workdir":
				req.Workdir = t.TempDir()
			case "policy":
				req.Permission = "auto"
			case "mcp-binding":
				req.MCPServers = []acp.MCPServer{acp.HTTPMCPServer("node-tool", "http://127.0.0.1:9000/b/original-binding", nil)}
				record.ConfigHash = sessionConfigHash(req)
				req.MCPServers[0].URL = "http://127.0.0.1:9000/b/replacement-binding"
			case "coordinator":
				record.Authority.CoordinatorEpoch = 2
			case "start":
				cfg.SessionAuthorizer.(*sessionAuthorityTest).denyStart = true
			}
			saveResumeFixture(t, cfg, record)
			s := NewServer(cfg)
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer s.sessions.Close()
			if mode == "cancelled-open" {
				cancel := req
				cancel.ID, cancel.Action = "", nodewire.SessionActionCancelOpen
				if _, err := s.sessions.Do(t.Context(), "cluster-1", cancel); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
				t.Fatal("unsafe source resumed")
			}
			if len(s.sessions.sessions) != 0 {
				t.Fatal("refusal reserved a process")
			}
			archived, _, _ := s.sessions.readRecord(record.State.ID)
			if archived.ResumeTarget != "" {
				t.Fatal("refusal consumed original context")
			}
		})
	}
}

// An interrupted turn is the ordinary case after a service restart: the
// agent answered the prompt with a cancellation and its permission request
// died with the process. Both are settled outcomes, so the conversation
// must keep its native context instead of being stranded on this node.
func TestCancelledTurnStillResumesItsNativeContext(t *testing.T) {
	cfg, req, old := resumedFixture(t, buildMockAgent(t))
	cancelled := old.Commands["old-input"]
	cancelled.State, cancelled.Error = nodewire.SessionCommandCancelled, "agent canceled the turn: context canceled"
	old.Commands["old-input"] = cancelled
	old.State.Questions = []nodewire.SessionQuestion{{ID: "q1", CommandID: "old-input", State: nodewire.SessionQuestionInterrupted}}
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatalf("a settled cancellation blocked the resume: %v", err)
	}
	if state.ID == old.State.ID || state.Binding != req.Binding || state.InputAccepted != 0 {
		t.Fatalf("resume did not reserve a fresh execution: %+v", state)
	}
	current, _, err := s.sessions.readRecord(state.ID)
	if err != nil || current.UpstreamID != old.UpstreamID || current.ResumedFrom != old.State.ID {
		t.Fatalf("resume started a different native conversation: %+v %v", current, err)
	}
}

// A refusal the node decides before reserving anything says so, so the
// coordinator can retry instead of quarantining a writer that never ran.
func TestOpenRefusedBeforeReservingSaysNothingStarted(t *testing.T) {
	cfg, req, record := resumedFixture(t, "/must-not-start")
	c := record.Commands["old-input"]
	c.State = nodewire.SessionCommandUncertain
	record.Commands["old-input"] = c
	saveResumeFixture(t, cfg, record)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	_, err := s.sessions.Do(t.Context(), "cluster-1", req)
	var refused *nodewire.SessionOpenNotStarted
	if !errors.As(err, &refused) {
		t.Fatalf("an unreserved refusal did not report that nothing started: %v", err)
	}
	if len(s.sessions.sessions) != 0 {
		t.Fatal("refusal reserved a process")
	}
}

func TestNativeResumeDoesNotFallBackToANewConversation(t *testing.T) {
	cfg, req, record := resumedFixture(t, buildMockAgent(t))
	spec := cfg.Harnesses["mock"]
	spec.Env = []string{"MOCKAGENT_REJECT_LOAD=1"}
	cfg.Harnesses["mock"] = spec
	saveResumeFixture(t, cfg, record)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil || !strings.Contains(err.Error(), "session/load") {
		t.Fatalf("unsupported resume invented a fresh conversation: %v", err)
	}
	id := nodewire.SessionOpenID(req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness)
	failed, _, err := s.sessions.readRecord(id)
	if err != nil || !failed.State.ProcessStopped || failed.State.InputAccepted != 0 {
		t.Fatalf("failed native load did not persist confirmed exit before returning: %+v %v", failed, err)
	}
	inspect := req
	inspect.ID, inspect.Action = "", nodewire.SessionActionCancelOpen
	state, err := s.sessions.Do(t.Context(), "cluster-1", inspect)
	if err != nil || !state.ProcessStopped || state.InputAccepted != 0 {
		t.Fatalf("failed load has no safe cancellation receipt: %+v %v", state, err)
	}
}

func TestNativeResumePreparationHasACancellableStoppedReceipt(t *testing.T) {
	cfg, req, source := resumedFixture(t, buildMockAgent(t))
	source = saveResumeFixture(t, cfg, source)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	source, _, err := s.sessions.readRecord(source.State.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := nodewire.SessionOpenID(req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness)
	broker, _ := permission.New("read")
	s.sessions.mu.Lock()
	prepared, _, err := s.sessions.prepareOwnedSession(t.Context(), id, "reserved-open", &req, cfg.Harnesses["mock"], broker, &source)
	s.sessions.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.host.Close()
	// The caller fails before native start, e.g. acquiring plugin runtime use.
	// Its published source claim and destination must remain safely cancellable.
	inspect := req
	inspect.ID, inspect.Action = "", nodewire.SessionActionCancelOpen
	stopped, err := s.sessions.Do(t.Context(), "cluster-1", inspect)
	if err != nil || !stopped.ProcessStopped || stopped.State != nodewire.SessionClosed || stopped.InputAccepted != 0 {
		t.Fatalf("preparation could not be cancelled: %+v %v", stopped, err)
	}
	req.CommandID = "later/open"
	req.Binding.TaskID = "task-3"
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || state.InputAccepted != 0 {
		t.Fatalf("stopped preparation stranded native history: %+v %v", state, err)
	}
}

func TestArchivedOpenObservationRequiresExactConfiguration(t *testing.T) {
	cfg, req, record := resumedFixture(t, "/must-not-start")
	req.Binding = record.State.Binding
	req.CommandID = record.OpenID
	saveResumeFixture(t, cfg, record)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	if state, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil || state.ID != record.State.ID || state.State != nodewire.SessionInterrupted {
		t.Fatalf("exact observation failed: %+v %v", state, err)
	}
	for _, mode := range []string{"command", "harness", "workdir", "policy"} {
		changed := req
		switch mode {
		case "command":
			changed.CommandID = "other/open"
		case "harness":
			changed.Harness = "other"
		case "workdir":
			changed.Workdir = t.TempDir()
		case "policy":
			changed.Permission = "auto"
		}
		if _, err := s.sessions.Do(t.Context(), "cluster-1", changed); err == nil {
			t.Fatalf("changed %s accepted as archived observation", mode)
		}
	}
}
