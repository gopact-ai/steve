package node

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceChangeAppliesToNextMCPLaunchWithoutRestart(t *testing.T) {
	broker, socket, _ := brokerOnSocket(t)
	before, after := t.TempDir(), t.TempDir()
	broker.SetWorkspaceRoot(before)
	broker.SetServers(map[string]MCPSpec{"cwd": {Command: "sh", Args: []string{"-c", "pwd; while IFS= read -r line; do pwd; done"}}})
	s := NewServer(ServerConfig{Name: "worker", WorkspaceRoot: before})
	s.broker = localBroker{b: broker}
	readCWD := func(attempt string) string {
		t.Helper()
		binding, err := broker.Bind("cwd", attempt, "test")
		if err != nil {
			t.Fatal(err)
		}
		defer broker.Release(attempt)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var out bytes.Buffer
		if err := LaunchBinding(ctx, socket, binding.Args[3], strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String())
	}
	wantBefore, _ := filepath.EvalSymlinks(before)
	wantAfter, _ := filepath.EvalSymlinks(after)
	if got := readCWD("before"); got != wantBefore {
		t.Fatalf("initial cwd = %q, want %q", got, wantBefore)
	}
	// Keep one real MCP process connected across the update. The new default
	// must neither change its cwd nor terminate its existing binding.
	binding, err := broker.Bind("cwd", "retained", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Release("retained")
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(conn, binding.Args[3]); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	checkRetained := func() {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != wantBefore {
			t.Fatalf("retained process cwd=%q error=%v, want %q", line, err, wantBefore)
		}
	}
	checkRetained()
	if err := s.SetWorkspaceRoot(after); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(conn, "check existing process"); err != nil {
		t.Fatal(err)
	}
	checkRetained()
	if got := readCWD("after"); got != wantAfter {
		t.Fatalf("new MCP launch retained stale cwd %q, want %q", got, wantAfter)
	}
}

func TestBrokerInitializationSharesSettingsPublicationBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
		MCPServers: map[string]MCPSpec{"cwd": {Command: "sh", Args: []string{"-c", "pwd; cat"}}}})
	s.ctx = ctx
	t.Cleanup(func() { cancel(); s.backgroundWG.Wait() })
	s.settingsMu.Lock()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); done <- s.startBroker() }()
	<-started
	early := false
	select {
	case err := <-done:
		early = true
		t.Errorf("broker initialized outside settings publication lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	root := t.TempDir()
	updated := make(chan error, 1)
	go func() { updated <- s.SetWorkspaceRoot(root) }()
	s.settingsMu.Unlock()
	if !early {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	broker, ok := s.currentBroker().(localBroker)
	if !ok || broker.b.workspaceRoot() != root {
		t.Fatal("broker initialization retained pre-update workspace")
	}
}
