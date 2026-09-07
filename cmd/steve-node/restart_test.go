//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestNodeCommandReexecutesAndKeepsDurableCommandIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and restarts a real service process")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "steve-node")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build node: %v %s", err, out)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	configPath := filepath.Join(dir, "node.json")
	cfg := node.ServerConfig{Name: "child", Listen: address, Token: "test-token", StateDir: filepath.Join(dir, "state"), Harnesses: map[string]node.HarnessSpec{"cat": {Command: "/bin/cat"}}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "node.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command := exec.Command(binary, "-config", configPath)
	command.Env = append(os.Environ(), "STEVE_RESTART_TEST_VALUE=kept-through-exec")
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-exited
		}
		if t.Failed() {
			raw, _ := os.ReadFile(log.Name())
			t.Log(string(raw))
		}
	})
	registry := node.NewRegistry("owner-hub", map[string]node.Config{"n": {Addr: address, Token: "test-token", DialTimeout: 100 * time.Millisecond}})
	t.Cleanup(registry.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	wait := func(commandID string, predicate func(nodewire.RestartStatus) bool) nodewire.RestartStatus {
		t.Helper()
		for {
			status, err := registry.RestartStatus(ctx, "n", commandID)
			if err == nil && predicate(status) {
				return status
			}
			select {
			case <-ctx.Done():
				t.Fatalf("service did not reach expected restart state: %+v %v", status, err)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	before := wait("", func(s nodewire.RestartStatus) bool { return s.State == "idle" && s.Supported })
	for _, broken := range [][]byte{[]byte(`{"invalid":"disk config"`), []byte(`{"name":"child","token":"changed-secret","listen":"` + address + `","state_dir":"` + cfg.StateDir + `","harnesses":{"cat":{"command":"/bin/cat"}}}`)} {
		if err := os.WriteFile(configPath, broken, 0600); err != nil {
			t.Fatal(err)
		}
		if status, err := registry.Restart(ctx, "n", "one-restart"); !errors.Is(err, nodewire.ErrRestartPreflight) || status.State == "accepted" {
			t.Fatal("invalid or disconnected configuration accepted", status, err)
		}
		if _, err := registry.RestartStatus(ctx, "n", "one-restart"); !errors.Is(err, nodewire.ErrRestartNotFound) {
			t.Fatal("rejected restart persisted an accepted receipt", err)
		}
		status, err := registry.RestartStatus(ctx, "n", "")
		if err != nil || status.Incarnation != before.Incarnation {
			t.Fatal("preflight stopped the healthy process", status, err)
		}
		output, err := registry.Exec(ctx, "n", "", "printf running")
		if err != nil || output != "running" {
			t.Fatal("preflight failure left work admission closed", output, err)
		}
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	accepted, err := registry.Restart(ctx, "n", "one-restart")
	if err != nil || accepted.State != "accepted" {
		t.Fatal("restart response lost before exec", accepted, err)
	}
	after := wait("one-restart", func(s nodewire.RestartStatus) bool { return s.State == "restarted" })
	if after.Incarnation == before.Incarnation || after.PreviousIncarnation != before.Incarnation {
		t.Fatal("restart only disconnected transport", before, after)
	}
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("reexec did not preserve service process", err)
	}
	result, err := registry.Exec(ctx, "n", "", `printf '%s' "$STEVE_RESTART_TEST_VALUE"`)
	if err != nil || strings.TrimSpace(result) != "kept-through-exec" {
		t.Fatal("restart did not retain environment", result, err)
	}
	for range 3 {
		replay, err := registry.Restart(ctx, "n", "one-restart")
		if err != nil || replay.State != "restarted" || replay.Incarnation != after.Incarnation {
			t.Fatal("same command triggered another restart", replay, err)
		}
	}
	latest := wait("", func(s nodewire.RestartStatus) bool { return s.State == "restarted" })
	if latest.CommandID != "one-restart" || latest.Incarnation != after.Incarnation {
		t.Fatal(latest)
	}
}
