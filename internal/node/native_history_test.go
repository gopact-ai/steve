package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func importedNodeFixture(t *testing.T) (*Server, nativehistory.Reference, string) {
	t.Helper()
	state, source, work := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(source, "sessions/2026/09/14/rollout-selected.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": "selected-native", "cwd": work}})
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	src := nativehistory.Source{Harness: "codex", Home: source}
	list, err := nativehistory.List(t.Context(), src)
	if err != nil || len(list) != 1 {
		t.Fatalf("history: %+v %v", list, err)
	}
	ref, err := nativehistory.Snapshot(t.Context(), filepath.Join(state, "native-imports"), nativehistory.ImportRequest{CommandID: "selected", Source: src, NativeID: list[0].NativeID, Revision: list[0].Revision})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(ServerConfig{Name: "worker", StateDir: state, WorkspaceRoot: work, Harnesses: map[string]HarnessSpec{"codex": {Command: buildMockAgent(t)}}, SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.sessions.Close() })
	return server, ref, path
}

func TestNativeLoadFailureRetainsInterruptedExecutionWithoutFreshFallback(t *testing.T) {
	s, ref, _ := importedNodeFixture(t)
	cfg := s.conf()
	spec := cfg.Harnesses["codex"]
	spec.Env = append(spec.Env, "MOCKAGENT_REJECT_LOAD=1")
	cfg.Harnesses["codex"] = spec
	req := nodeSessionRequest("open")
	req.Harness, req.Workdir, req.CommandID = "codex", ref.SourceWorkdir, "failed-import"
	req.NativeImport, req.Binding.NativeImportID = ref.Clone(), ref.ID
	got, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err == nil || !strings.Contains(err.Error(), "session/load") {
		t.Fatalf("load failure hidden: %+v %v", got, err)
	}
	if got.State != nodewire.SessionInterrupted || got.InputAccepted != 0 || got.NativeImport == nil {
		t.Fatalf("failed import became a fresh execution: %+v", got)
	}
	record, exists, err := s.sessions.readRecord(got.ID)
	if err != nil || !exists || record.UpstreamID != "" || record.State.NativeImport.ID != ref.ID {
		t.Fatalf("failed native open lost provenance: %+v %v", record, err)
	}
}

func TestImportedHistoryOpensTheSelectedNativeSessionUnderANewManagedIdentity(t *testing.T) {
	s, ref, sourcePath := importedNodeFixture(t)
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	req := nodeSessionRequest("open")
	req.Harness, req.Workdir, req.CommandID = "codex", ref.SourceWorkdir, "import-open"
	req.NativeImport, req.Binding.NativeImportID = ref.Clone(), ref.ID
	opened, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(opened.ID, "ns_") || opened.NativeImport == nil || *opened.NativeImport != ref || opened.InputAccepted != 0 {
		t.Fatalf("managed import: %+v", opened)
	}
	record, exists, err := s.sessions.readRecord(opened.ID)
	if err != nil || !exists || record.UpstreamID != ref.NativeID {
		t.Fatalf("opened a fresh native session: %+v %v", record, err)
	}
	if repeat, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil || repeat.ID != opened.ID {
		t.Fatalf("lost open receipt: %+v %v", repeat, err)
	}
	req.NativeImport.NativeID = "different-history"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("idempotent open accepted different history")
	}
	after, err := os.ReadFile(sourcePath)
	if err != nil || string(after) != string(before) {
		t.Fatal("native load modified source history")
	}
	copyPath := filepath.Join(s.conf().StateDir, "native-runtimes", opened.ID, "home", "sessions/2026/09/14/rollout-selected.jsonl")
	if copied, err := os.ReadFile(copyPath); err != nil || string(copied) != string(before) {
		t.Fatal("runtime did not receive selected history")
	}
}

func TestNativeImportRejectsMissingBindingAndWorkspaceRemapping(t *testing.T) {
	s, ref, _ := importedNodeFixture(t)
	req := nodeSessionRequest("open")
	req.Harness, req.Workdir, req.CommandID, req.NativeImport = "codex", ref.SourceWorkdir, "invalid-import", ref.Clone()
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("uncommitted import accepted")
	}
	req.Binding.NativeImportID = ref.ID
	req.Workdir = t.TempDir()
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("history resumed in another workspace")
	}
	if len(s.sessions.sessions) != 0 {
		t.Fatal("rejected import created an execution")
	}
}
