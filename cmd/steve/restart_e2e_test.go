//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

// isolatedHub is a real Hub built from this tree and run on its own port,
// state and home, so a restart can be asked for and watched end to end.
type isolatedHub struct {
	t       *testing.T
	dir     string
	binary  string
	config  string
	raw     []byte
	url     string
	command *exec.Cmd
	client  *http.Client
}

func startIsolatedHub(t *testing.T) *isolatedHub {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "steve")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserve.Addr().String()
	reserve.Close()
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"projects": map[string]any{"p": map[string]any{"home": map[string]string{"path": work}}}, "agents": map[string]any{"mock": map[string]any{"harness": "mock", "default": true}}, "harnesses": map[string]any{"mock": map[string]string{"command": "/bin/cat"}}, "gateway": map[string]any{"owner_id": "fixture-owner", "hub_id": "restart-fixture", "default_channel": "console", "default_project": "p", "state_path": filepath.Join(dir, "state", "state.json"), "home_path": filepath.Join(dir, "home"), "read_model_addr": addr, "read_model_token": "fixture"}}
	raw, _ := json.Marshal(cfg)
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "hub.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	command := exec.Command(binary, "run", "-config", config)
	command.Dir = dir
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg")}
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
		case <-time.After(6 * time.Second):
			_ = command.Process.Kill()
			<-exited
		}
		if t.Failed() {
			raw, _ := os.ReadFile(log.Name())
			t.Log(string(raw))
		}
	})
	return &isolatedHub{t: t, dir: dir, binary: binary, config: config, raw: raw,
		url: "http://" + addr + "/console/services/hub/restart", command: command, client: &http.Client{Timeout: 2 * time.Second}}
}

func (h *isolatedHub) request(method, path string, body []byte) (consoleapi.RestartOperation, error) {
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	if err != nil {
		return consoleapi.RestartOperation{}, err
	}
	req.Header.Set("Authorization", "Bearer fixture")
	r, err := h.client.Do(req)
	if err != nil {
		return consoleapi.RestartOperation{}, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return consoleapi.RestartOperation{}, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var op consoleapi.RestartOperation
	err = json.NewDecoder(r.Body).Decode(&op)
	return op, err
}

func (h *isolatedHub) wait(id, state string) consoleapi.RestartOperation {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		op, err := h.request("GET", h.url+"?command_id="+id, nil)
		if err == nil && string(op.State) == state {
			return op
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatal("Hub did not reach " + state)
	return consoleapi.RestartOperation{}
}

func TestHubReexecKeepsPIDConfigAndDurableRestartIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and restarts a real isolated Hub")
	}
	hub := startIsolatedHub(t)
	command, cfgp, raw, url, request, wait := hub.command, hub.config, hub.raw, hub.url, hub.request, hub.wait
	before := wait("", "idle")
	if err := os.WriteFile(cfgp, []byte(`{"invalid":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := request("POST", url, []byte(`{"command_id":"invalid-config"}`)); err == nil {
		t.Fatal("invalid saved config shut down healthy Hub")
	}
	if still := wait("", "idle"); still.Incarnation != before.Incarnation {
		t.Fatal("invalid config was restarted")
	}
	if err := os.WriteFile(cfgp, raw, 0600); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"command_id":"restart-once"}`)
	accepted, err := request("POST", url, body)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != "accepted" || accepted.Incarnation != before.Incarnation {
		t.Fatalf("accept=%+v", accepted)
	}
	after := wait("restart-once", "restarted")
	if after.Incarnation == before.Incarnation {
		t.Fatal("disconnect falsely counted as restart")
	}
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("reexec lost process PID: %v", err)
	}
	replay, err := request("POST", url, body)
	if err != nil || replay.Incarnation != after.Incarnation || replay.State != "restarted" {
		t.Fatalf("replay=%+v %v", replay, err)
	}
	unchanged, err := os.ReadFile(cfgp)
	if err != nil || !bytes.Equal(unchanged, raw) {
		t.Fatal("restart altered configured state")
	}
}

// A replaced program is what the service comes back on. The restart is
// asked for the way the desktop launcher asks for it, and the proof is
// that the process keeps its PID while running the file that replaced it.
func TestHubWhenIdleRestartRunsTheReplacedProgram(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and restarts a real isolated Hub")
	}
	hub := startIsolatedHub(t)
	hub.wait("", "idle")
	marker := filepath.Join(hub.dir, "replaced")
	replacement := "#!/bin/sh\nprintf '%s' \"$*\" > " + marker + "\nexec sleep 120\n"
	if err := os.Remove(hub.binary); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hub.binary, []byte(replacement), 0700); err != nil {
		t.Fatal(err)
	}
	scheduled, err := hub.request("POST", hub.url, []byte(`{"command_id":"desktop-upgrade","mode":"when-idle"}`))
	if err != nil {
		t.Fatal(err)
	}
	if scheduled.Mode != consoleapi.RestartWhenIdle || scheduled.State != "draining" {
		t.Fatalf("scheduled=%+v", scheduled)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(marker); err == nil {
			// The replacement was handed the arguments the service was
			// started with, so it is the same service and not a new one.
			if !strings.Contains(string(raw), "run") {
				t.Fatalf("the replaced program was started differently: %q", raw)
			}
			if err := hub.command.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("the upgrade lost the process: %v", err)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the waiting restart never ran the replaced program")
}
