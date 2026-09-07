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
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestHubReexecKeepsPIDConfigAndDurableRestartIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and restarts a real isolated Hub")
	}
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
	cfgp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgp, raw, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "hub.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command := exec.Command(binary, "run", "-config", cfgp)
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
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + addr + "/console/services/hub/restart"
	request := func(method, path string, body []byte) (consoleapi.RestartOperation, error) {
		req, err := http.NewRequest(method, path, bytes.NewReader(body))
		if err != nil {
			return consoleapi.RestartOperation{}, err
		}
		req.Header.Set("Authorization", "Bearer fixture")
		r, err := client.Do(req)
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
	wait := func(id, state string) consoleapi.RestartOperation {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			op, err := request("GET", url+"?command_id="+id, nil)
			if err == nil && op.State == state {
				return op
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("Hub did not reach " + state)
		return consoleapi.RestartOperation{}
	}
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
