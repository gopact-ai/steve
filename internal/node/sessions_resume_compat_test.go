package node

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestNativeResumeFollowsLongHistoryAndRejectsCycles(t *testing.T) {
	cfg, req, old := resumedFixture(t, "/must-not-start")
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	store, err := s.sessions.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 80 {
		old.State.ID = "ns_" + sessionHash(fmt.Sprint(i))
		old.ResumeTarget = "ns_" + sessionHash(fmt.Sprint(i+1))
		if i == 0 {
			req.ID = old.State.ID
		}
		if i == 79 {
			old.ResumeTarget = ""
		}
		saveSessionRecordsFixture(t, store, old)
	}
	got, err := s.sessions.resumeSourceLocked(req, "next-execution")
	if err != nil || got.State.ID != old.State.ID {
		t.Fatalf("valid long chain rejected: %+v %v", got, err)
	}
	old.ResumeTarget = req.ID
	saveSessionRecordsFixture(t, store, old)
	if _, err := s.sessions.resumeSourceLocked(req, "next-execution"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle accepted: %v", err)
	}
}

func TestNativeResumeRetriesFromOldestSourceAcrossHandoffs(t *testing.T) {
	cfg, req, old := resumedFixture(t, buildMockAgent(t))
	saveResumeFixture(t, cfg, old)
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { s.sessions.Close() }()
	previous := old.State.ID
	for i := range 3 {
		req.Binding.TaskID, req.CommandID = fmt.Sprintf("task-%d", i+2), fmt.Sprintf("open-%d", i+2)
		first, err := s.sessions.Do(t.Context(), "cluster-1", req)
		if err != nil || first.ID == previous || first.ContextID != old.State.ID {
			t.Fatalf("handoff %d lost original context: %+v %v", i, first, err)
		}
		for _, closed := range []bool{false, true} {
			if closed {
				stop := req
				stop.ID, stop.Action = first.ID, nodewire.SessionActionClose
				if _, err := s.sessions.Do(t.Context(), "cluster-1", stop); err != nil {
					t.Fatal(err)
				}
				s.sessions.Close()
				s = NewServer(cfg)
				if err := s.startSessions(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.sessions.Do(t.Context(), "cluster-1", req)
			if err != nil || got.ID != first.ID || got.ProcessStopped != closed || got.InputAccepted != 0 {
				t.Fatalf("handoff %d retry closed=%v did not observe reserved target: %+v %v", i, closed, got, err)
			}
		}
		previous = first.ID
	}
}

// Native history is not asked of a node whose advert lacks it, and the
// refusal says why: the node's platform cannot store native history.
func TestNativeHistoryRefusalSaysWhyTheNodeLacksIt(t *testing.T) {
	r := advertisingRegistry(t, nodewire.Advert{Node: "worker"})
	if _, err := r.NativeHistory(t.Context(), "worker", nativehistory.Source{}); err == nil || !strings.Contains(err.Error(), "its platform cannot store native history") {
		t.Errorf("native history = %v, want the platform named", err)
	}
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Binding.NativeImportID = "import-1"
	_, err := r.NodeSession(t.Context(), "worker", req)
	var unsent *nodewire.SessionNotDispatched
	if !errors.As(err, &unsent) || !strings.Contains(err.Error(), "its platform cannot store native history") {
		t.Fatalf("native import = %v, want it unsent for the platform", err)
	}
}

// advertisingRegistry is a registry connected to one node that carries
// advert and answers no stream.
func advertisingRegistry(t *testing.T, advert nodewire.Advert) *Registry {
	t.Helper()
	client, server := net.Pipe()
	remote := nodewire.NewMux(server, false)
	t.Cleanup(func() { _ = remote.Close() })
	r := NewRegistry("hub", map[string]Config{advert.Node: {Addr: "test", Token: "test"}})
	r.live[advert.Node] = &conn{name: advert.Node, mux: nodewire.NewMux(client, true), advert: advert}
	t.Cleanup(r.Close)
	return r
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
