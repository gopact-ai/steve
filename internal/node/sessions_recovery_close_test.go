package node

import (
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestRecoveryIdleCloseReplaysItsPersistedWholeProcessProofAfterRestart(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority}
	server := NewServer(cfg)
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.sessions.Close()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID = "mock", t.TempDir(), "exact-open"
	opened, err := server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID, req.Action = opened.ID, nodewire.SessionActionClose
	stopped, err := server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || !stopped.ProcessStopped || stopped.State != nodewire.SessionClosed {
		t.Fatalf("idle close did not issue real whole-process proof: %+v %v", stopped, err)
	}
	server.sessions.Close()
	reopened := NewServer(cfg)
	if err := reopened.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer reopened.sessions.Close()
	for range 2 {
		replay, err := reopened.sessions.Do(t.Context(), "cluster-1", req)
		if err != nil || !replay.ProcessStopped || replay.ID != stopped.ID || replay.Binding != stopped.Binding || replay.ContextID != stopped.ContextID || replay.Sequence != stopped.Sequence {
			t.Fatalf("restarted close lost its persisted machine proof: %+v %v", replay, err)
		}
	}
	for _, mode := range []string{"binding", "cluster", "command"} {
		bad := req
		switch mode {
		case "binding":
			bad.Binding.TaskEpoch++
		case "cluster":
			bad.Authority.ClusterID = "another-cluster"
		case "command":
			bad.CommandID = "unknown-command"
		}
		if _, err := reopened.sessions.Do(t.Context(), "cluster-1", bad); err == nil {
			t.Fatalf("%s mismatch was promoted to exact idle-close proof", mode)
		}
	}
	record, found, err := reopened.sessions.readRecord(req.ID)
	if err != nil || !found || !reflect.DeepEqual(record.State.Binding, stopped.Binding) {
		t.Fatalf("bad retry changed original binding: %+v %v", record, err)
	}
}
