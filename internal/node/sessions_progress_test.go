package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

func progressSession(t *testing.T) *ownedSession {
	t.Helper()
	server := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir()})
	one := &ownedSession{service: &SessionService{server: server, ctx: t.Context()}, changed: make(chan struct{}), record: sessionRecord{Format: 1, State: nodewire.SessionState{ID: "ns_" + strings.Repeat("a", 64), State: "running"}, CurrentCommand: "input", Commands: map[string]nodewire.SessionCommand{"input": {ID: "input", State: "running", DispatchState: "dispatched"}}, CommandHashes: map[string]string{"input": "hash"}}}
	if err := one.commitLocked(one.record); err != nil {
		t.Fatal(err)
	}
	return one
}
func savedProgress(t *testing.T, one *ownedSession) sessionRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(one.service.directory(), one.record.State.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var out sessionRecord
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestNodeProgressBurstCoalescesDurableWrites(t *testing.T) {
	one := progressSession(t)
	base := one.record.State.Sequence
	for n := 1; n <= 200; n++ {
		if err := one.updateProgress("input", view.Progress{Answer: strings.Repeat("x", n)}); err != nil {
			t.Fatal(err)
		}
	}
	one.mu.Lock()
	count := one.record.State.Sequence - base
	one.mu.Unlock()
	if count >= 20 {
		t.Fatalf("200 text chunks forced %d durable writes", count)
	}
	deadline := time.After(2 * time.Second)
	for {
		one.mu.Lock()
		latest := one.record.State.Progress.Answer
		changed := one.changed
		one.mu.Unlock()
		if len(latest) == 200 {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("trailing progress never persisted without another chunk")
		}
	}
	if got := savedProgress(t, one); len(got.State.Progress.Answer) != 200 {
		t.Fatal("latest progress missing on disk")
	}
}
func TestNodeTerminalCommitIncludesPendingProgress(t *testing.T) {
	one := progressSession(t)
	for _, text := range []string{"first", "full final output"} {
		if err := one.updateProgress("input", view.Progress{Answer: text}); err != nil {
			t.Fatal(err)
		}
	}
	one.mu.Lock()
	next := one.copyLocked()
	next.State.State = "idle"
	command := next.Commands["input"]
	command.State = "completed"
	command.Settled = true
	command.Output = "full final output"
	next.Commands["input"] = command
	if err := one.commitLocked(next); err != nil {
		t.Fatal(err)
	}
	sequence := one.record.State.Sequence
	one.mu.Unlock()
	saved := savedProgress(t, one)
	if saved.State.Progress.Answer != command.Output || !saved.Commands["input"].Settled {
		t.Fatal("terminal receipt omitted latest output")
	}
	time.Sleep(300 * time.Millisecond)
	one.mu.Lock()
	defer one.mu.Unlock()
	if one.record.State.Sequence != sequence {
		t.Fatal("late progress timer mutated a completed command")
	}
}

func TestNodeProgressDoesNotCrossInputBoundary(t *testing.T) {
	one := progressSession(t)
	if err := one.updateProgress("previous", view.Progress{Answer: "stale"}); err != nil {
		t.Fatal(err)
	}
	if savedProgress(t, one).State.Progress.Answer != "" {
		t.Fatal("old command changed current progress")
	}
}

func TestNodeProgressPersistenceFailureQuarantinesSession(t *testing.T) {
	one := progressSession(t)
	if err := one.updateProgress("input", view.Progress{Answer: "first"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(one.service.directory(), one.record.State.ID+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	one.mu.Lock()
	changed := one.changed
	one.mu.Unlock()
	if err := one.updateProgress("input", view.Progress{Answer: "later"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("failed checkpoint did not wake observer")
	}
	one.mu.Lock()
	failure := one.failure
	one.mu.Unlock()
	if failure == nil {
		t.Fatal("checkpoint failure silently ignored")
	}
	if err := one.updateProgress("input", view.Progress{Answer: "retry"}); err == nil {
		t.Fatal("failed checkpoint allowed continued execution")
	}
}

// The real ACP subprocess and native session owner must preserve every chunk
// while checkpoint traffic stays independent of the model's chunk count.
func TestNodeACPStreamCoalescesWithoutLosingFinalOutput(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t), Env: []string{"MOCKAGENT_STREAM_CHUNKS=500"}}}, SessionAuthorizer: authority})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	req := nodeSessionRequest("open")
	req.Harness = "mock"
	req.Workdir = t.TempDir()
	req.CommandID = "open-stream"
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	base := state.Sequence
	req.ID = state.ID
	req.Action = "prompt"
	req.Text = "stream"
	req.CommandID = "stream-input"
	req.InputSequence = 1
	state, err = s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for state.Command == nil || !state.Command.Settled {
		if time.Now().After(deadline) {
			t.Fatal("native stream never completed")
		}
		req.Action = "poll"
		req.After = state.Sequence
		req.WaitMS = 100
		state, err = s.sessions.Do(t.Context(), "cluster-1", req)
		if err != nil {
			t.Fatal(err)
		}
	}
	want := strings.Repeat("stream-fragment\n", 500)
	if state.Command.Output != want || state.Progress.Answer != want {
		t.Fatal("coalescing lost streamed output")
	}
	if state.Sequence-base >= 100 {
		t.Fatalf("500 ACP chunks forced %d state checkpoints", state.Sequence-base)
	}
	one := s.sessions.sessions[state.ID]
	saved := savedProgress(t, one)
	if saved.Commands["stream-input"].Output != want || !saved.Commands["stream-input"].Settled {
		t.Fatal("full settled output was not durable")
	}
}
