package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countedCommand wraps a binary in a script that records every start, so a
// test can tell how many processes a sequence of requests cost.
func countedCommand(t *testing.T, bin string) (string, func() int) {
	t.Helper()
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	script := filepath.Join(dir, "counted.sh")
	body := "#!/bin/sh\necho start >> " + starts + "\nexec " + bin + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, func() int {
		data, err := os.ReadFile(starts)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "start\n")
	}
}

func TestNodeCapabilitiesAreProbedOncePerHarness(t *testing.T) {
	counted, started := countedCommand(t, buildMockAgent(t))
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: counted}}, SessionAuthorizer: authority})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.startSessions(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	ask := func() bool {
		t.Helper()
		req := nodeSessionRequest("capabilities")
		req.Harness = "mock"
		state, err := s.sessions.Do(ctx, "cluster-1", req)
		if err != nil {
			t.Fatal(err)
		}
		return state.SupportsHTTPMCP
	}
	// The hub asks before every turn; the binary is started once for it.
	first := ask()
	if ask() != first {
		t.Fatal("the cached answer differs from the probe")
	}
	if n := started(); n != 1 {
		t.Fatalf("two capability questions started %d processes", n)
	}
	// A real session is the agent's own answer, and refreshes the cache.
	open := nodeSessionRequest("open")
	open.Harness, open.Workdir, open.CommandID = "mock", t.TempDir(), "open-1"
	state, err := s.sessions.Do(ctx, "cluster-1", open)
	if err != nil {
		t.Fatal(err)
	}
	if state.SupportsHTTPMCP != first {
		t.Fatalf("session says %v, probe said %v", state.SupportsHTTPMCP, first)
	}
	if ask() != first || started() != 2 {
		t.Fatalf("after the open, %d processes were started", started())
	}
	// Another binary is another question.
	if _, ok := s.sessions.cachedCapabilities("mock", "other-binary"); ok {
		t.Fatal("an answer for a different binary was reused")
	}
}
