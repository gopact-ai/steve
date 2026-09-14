package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestNativeResumeFollowsLongHistoryAndRejectsCycles(t *testing.T) {
	cfg, req, old := resumedFixture(t, "/must-not-start")
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	for i := range 80 {
		old.State.ID = "ns_" + sessionHash(fmt.Sprint(i))
		old.ResumeTarget = "ns_" + sessionHash(fmt.Sprint(i+1))
		if i == 0 {
			req.ID = old.State.ID
		}
		if i == 79 {
			old.ResumeTarget = ""
		}
		saveResumeFixture(t, cfg, old)
	}
	got, err := s.sessions.resumeSourceLocked(req, "next-execution")
	if err != nil || got.State.ID != old.State.ID {
		t.Fatalf("valid long chain rejected: %+v %v", got, err)
	}
	old.ResumeTarget = req.ID
	saveResumeFixture(t, cfg, old)
	if _, err := s.sessions.resumeSourceLocked(req, "next-execution"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle accepted: %v", err)
	}
}

func TestNativeResumeIsNotSentToOlderNode(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "worker", Token: "resume-feature", StateDir: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	r := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "resume-feature"}})
	defer r.Close()
	c, err := r.connect(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	adv := c.getAdvert()
	if !nodewire.HasFeature(adv.Features, nodewire.FeatureNativeResume) {
		t.Fatal("new node omitted resume feature")
	}
	adv.Features = slices.DeleteFunc(adv.Features, func(s string) bool { return s == nodewire.FeatureNativeResume })
	c.setAdvert(adv)
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.ID = "ns_" + strings.Repeat("0", 64)
	_, err = r.NodeSession(t.Context(), "worker", req)
	var unsent *nodewire.SessionNotDispatched
	if !errors.As(err, &unsent) || !strings.Contains(err.Error(), "update steve-node") {
		t.Fatalf("old node received resume: %v", err)
	}
}

func TestImportedNativeSessionRetainsPostImportHistoryAcrossRestart(t *testing.T) {
	s, ref, original := importedNodeFixture(t)
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID = "codex", ref.SourceWorkdir, "import-open"
	req.NativeImport, req.Binding.NativeImportID = ref.Clone(), ref.ID
	first, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(s.conf().StateDir, "native-runtimes", first.ID, "home")
	path := filepath.Join(home, "sessions/2026/09/14/rollout-selected.jsonl")
	before, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	updated := append(slices.Clone(before), []byte("post-import-context\n")...)
	if err := os.WriteFile(path, updated, 0600); err != nil {
		t.Fatal(err)
	}
	s.sessions.Close()
	restarted := NewServer(s.conf())
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	req.ID, req.CommandID, req.Binding.TaskID = first.ID, "resume-open", "next-task"
	second, err := restarted.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := restarted.sessions.readRecord(second.ID)
	if err != nil || record.RuntimeSession != first.ID || record.UpstreamID != ref.NativeID || second.ID == first.ID {
		t.Fatalf("import continuation replaced context: %+v %v", record, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(updated) {
		t.Fatal("post-import messages were replaced", err)
	}
	if _, err := os.Stat(filepath.Join(s.conf().StateDir, "native-runtimes", second.ID)); !os.IsNotExist(err) {
		t.Fatal("resume created a fresh native home", err)
	}
	if got, err := os.ReadFile(original); err != nil || string(got) != string(before) {
		t.Fatal("source transcript changed", err)
	}
}
