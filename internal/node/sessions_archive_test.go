package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestNodeSessionClosedArchiveDoesNotConsumeLiveCapacity(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority}
	dir := filepath.Join(cfg.StateDir, "node-sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	req := nodeSessionRequest("open")
	req.Harness = "mock"
	req.Workdir = t.TempDir()
	var archivedID string
	for i := 0; i < 1024; i++ {
		req.CommandID = fmt.Sprintf("retired-open-%d", i)
		id := "ns_" + sessionHash([]string{req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness})
		archivedID = id
		hash := sessionHash(struct {
			Binding                      nodewire.SessionBinding
			Harness, Workdir, Permission string
			Servers                      any
		}{req.Binding, req.Harness, req.Workdir, "read", nil})
		record := sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, OpenID: req.CommandID, OpenHash: hash, ConfigHash: sessionConfigHash(req), State: nodewire.SessionState{ID: id, Binding: req.Binding, Harness: "mock", State: "closed", ProcessStopped: true, Sequence: 5}, Commands: map[string]nodewire.SessionCommand{"completed-input": {ID: "completed-input", InputSequence: 1, State: "completed", Output: "original result", Settled: true, ProcessStopped: true}}, CommandHashes: map[string]string{}, CurrentCommand: "completed-input"}
		raw, _ := json.Marshal(record)
		if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	if len(s.sessions.sessions) != 0 {
		t.Fatal("archived records occupied live session capacity")
	}
	inspect := nodeSessionRequest("attach")
	inspect.ID = archivedID
	inspect.CommandID = "completed-input"
	state, err := s.sessions.Do(t.Context(), "cluster-1", inspect)
	if err != nil || state.State != "closed" || state.Command == nil || state.Command.Output != "original result" {
		t.Fatalf("archived receipt unavailable: %+v %v", state, err)
	}
	req.CommandID = "new-open-after-archive"
	if state, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil || state.State != "idle" {
		t.Fatalf("retired sessions exhausted node: %+v %v", state, err)
	}
	inspect.Action = "prompt"
	inspect.Text = "never restart"
	inspect.CommandID = "new-input"
	inspect.InputSequence = 2
	if _, err := s.sessions.Do(t.Context(), "cluster-1", inspect); err == nil {
		t.Fatal("cold session started a new native prompt")
	}
}

func TestNodeSessionColdInterruptedRecordDoesNotProveOldProcessStopped(t *testing.T) {
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}}
	req := nodeSessionRequest("attach")
	req.ID = "ns_" + sessionHash("unknown-native")
	req.CommandID = "old-input"
	record := sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, State: nodewire.SessionState{ID: req.ID, Binding: req.Binding, State: "running"}, Commands: map[string]nodewire.SessionCommand{"old-input": {ID: "old-input", InputSequence: 1, State: "running"}}, CommandHashes: map[string]string{}, CurrentCommand: "old-input"}
	dir := filepath.Join(cfg.StateDir, "node-sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(dir, req.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	if len(s.sessions.sessions) != 0 {
		t.Fatal("unattachable native record retained a live slot")
	}
	if s.sessions.processesStopped() || s.processesStopped("cluster-1") {
		t.Fatal("missing callbacks were mistaken for physical process exit")
	}
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || state.State != "interrupted" || state.Command.State != "uncertain" || state.ProcessStopped {
		t.Fatalf("cold uncertainty lost: %+v %v", state, err)
	}
	req.Action = "abort"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("archived unknown process returned a false abort receipt")
	}
}

func TestNodeSessionRestartPreservesDispatchEvidenceWithoutInventingIt(t *testing.T) {
	for _, dispatch := range []string{"", "not-dispatched", "dispatched"} {
		t.Run(map[string]string{"": "unknown", "not-dispatched": "not-dispatched", "dispatched": "dispatched"}[dispatch], func(t *testing.T) {
			cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}}
			req := nodeSessionRequest("attach")
			req.ID = "ns_" + sessionHash(dispatch)
			req.CommandID = "original-input"
			record := sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, State: nodewire.SessionState{ID: req.ID, Binding: req.Binding, State: "running"}, Commands: map[string]nodewire.SessionCommand{"original-input": {ID: "original-input", InputSequence: 1, State: "accepted", DispatchState: dispatch}}, CommandHashes: map[string]string{}, CurrentCommand: "original-input"}
			dir := filepath.Join(cfg.StateDir, "node-sessions")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(record)
			if err := os.WriteFile(filepath.Join(dir, req.ID+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			s := NewServer(cfg)
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer s.sessions.Close()
			state, err := s.sessions.Do(t.Context(), "cluster-1", req)
			if err != nil || state.Command == nil || state.Command.DispatchState != dispatch || state.Command.State != "uncertain" || state.Command.ProcessStopped || state.ProcessStopped {
				t.Fatalf("restart changed dispatch or physical evidence: %+v %v", state, err)
			}
		})
	}
}
