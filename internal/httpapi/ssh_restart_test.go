package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// serviceSSH serves the upgrades and restarts of a real SSH service.
type serviceSSH struct {
	SSHService
	service *sshconnect.Service
}

func (s serviceSSH) SSHUpgrade(ctx context.Context, node string) (sshconnect.InstallResult, error) {
	return s.service.Upgrade(ctx, node)
}

func (s serviceSSH) SSHUpgradeStatus(ctx context.Context, node string) (sshconnect.InstallResult, error) {
	return s.service.UpgradeStatus(ctx, node)
}

func (s serviceSSH) SSHRestart(ctx context.Context, node string) (sshconnect.InstallResult, error) {
	return s.service.Restart(ctx, node)
}

func (s serviceSSH) SSHRestartStatus(ctx context.Context, node string) (sshconnect.RestartState, error) {
	return s.service.RestartStatus(ctx, node)
}

// peerMachine answers over SSH as a machine enrolled as a peer does: its
// probe finds the peer's state directory, and its restart script says it
// stopped the running peer and started it again.
type peerMachine struct{}

func (peerMachine) Run(_ context.Context, _ string, script string) (sshconnect.Output, error) {
	switch {
	case strings.Contains(script, "STEVE_RESTART"):
		return sshconnect.Output{Stdout: "Stopping peer process 4242.\nSTEVE_RESTART\trestarted\n"}, nil
	case strings.Contains(script, "STEVE_CHECK"):
		probe := "STEVE_CHECK\tos\tLinux\nSTEVE_CHECK\tarch\tx86_64\nSTEVE_CHECK\tuser\tremote-user\nSTEVE_CHECK\taddress\t192.0.2.7\n"
		for _, tool := range []string{"bash", "curl", "git", "nohup", "sha256sum", "shasum", "base64", "node", "npm"} {
			probe += "STEVE_CHECK\t" + tool + "\t1\n"
		}
		for _, candidate := range agenttools.Catalog() {
			probe += "STEVE_CHECK\t" + candidate.ID + "\t0\n"
		}
		return sshconnect.Output{Stdout: probe + "STEVE_CHECK\texisting_path_peer_state\t~/.steve-peer\nSTEVE_CHECK\texisting\t1\n"}, nil
	}
	return sshconnect.Output{}, errors.New("unexpected script")
}

func (peerMachine) Upload(context.Context, string, io.Reader) (sshconnect.Output, error) {
	return sshconnect.Output{}, errors.New("a restart uploads nothing")
}

func (peerMachine) Close() error { return nil }

// peerMachineRunner binds every alias to the peer machine; nothing runs
// outside a bound connection.
type peerMachineRunner struct{}

func (peerMachineRunner) Bind(context.Context, string, []string) (sshconnect.Connection, error) {
	return peerMachine{}, nil
}

func (peerMachineRunner) Run(context.Context, []string, string) (sshconnect.Output, error) {
	return sshconnect.Output{}, errors.New("unbound command")
}

func (peerMachineRunner) Upload(context.Context, []string, io.Reader) (sshconnect.Output, error) {
	return sshconnect.Output{}, errors.New("unbound upload")
}

// oneMachine is a backend that knows one machine, node-dev, reached as
// dev. An upgrade or restart of it can be held at its start until the
// test lets it go; the upgrade then fails, as there is no program to send.
type oneMachine struct {
	mu       sync.Mutex
	hold     chan struct{}
	entered  chan struct{}
	recorded []sshconnect.RestartRecord
}

func (b *oneMachine) Preview(context.Context, sshconnect.InstallRequest, sshconnect.CheckResult) (sshconnect.Template, error) {
	return sshconnect.Template{}, errors.New("not enrolling")
}

func (b *oneMachine) Register(context.Context, sshconnect.InstallRequest, sshconnect.CheckResult, string) (sshconnect.Registration, error) {
	return sshconnect.Registration{}, errors.New("not enrolling")
}

func (b *oneMachine) Verify(context.Context, string) error { return errors.New("not enrolling") }

func (b *oneMachine) Knows(_ context.Context, nodeID string) bool { return nodeID == "node-dev" }

// wait holds an operation at its start while the test holds the machine.
func (b *oneMachine) wait() {
	b.mu.Lock()
	hold, entered := b.hold, b.entered
	b.mu.Unlock()
	if hold != nil {
		close(entered)
		<-hold
	}
}

func (b *oneMachine) holdNext() (entered chan struct{}, release func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hold, b.entered = make(chan struct{}), make(chan struct{})
	hold := b.hold
	return b.entered, func() {
		b.mu.Lock()
		b.hold, b.entered = nil, nil
		b.mu.Unlock()
		close(hold)
	}
}

func (b *oneMachine) UpgradeTarget(context.Context, string) (sshconnect.UpgradeTarget, error) {
	b.wait()
	return sshconnect.UpgradeTarget{}, errors.New("no program to send")
}

func (b *oneMachine) Upgraded(context.Context, string) error { return nil }

func (b *oneMachine) RestartTarget(context.Context, string) (string, error) {
	b.wait()
	return "dev", nil
}

func (b *oneMachine) Restarted(context.Context, string) error { return nil }

func (b *oneMachine) RecordRestart(_ context.Context, record sshconnect.RestartRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recorded = append(b.recorded, record)
	return nil
}

func (b *oneMachine) records() []sshconnect.RestartRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sshconnect.RestartRecord(nil), b.recorded...)
}

// sshCall is one answer of the SSH API: its status and body.
type sshCall struct {
	status int
	body   []byte
}

func (c sshCall) step(t *testing.T) sshconnect.StepError {
	t.Helper()
	var body struct {
		Error string               `json:"error"`
		Step  sshconnect.StepError `json:"step"`
	}
	if err := json.Unmarshal(c.body, &body); err != nil || body.Error == "" {
		t.Fatalf("not a refusal: %d %s (%v)", c.status, c.body, err)
	}
	return body.Step
}

// A machine's peer is restarted through the API over a real SSH service:
// the restart answers with how it went and is readable while it runs and
// afterwards. A machine is upgraded or restarted one operation at a time,
// whichever came first; and a node ID no machine has is not found.
func TestSSHRestartOverTheAPIRunsOneOperationPerMachine(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(config, []byte("Host dev\nHostName dev.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &oneMachine{}
	service := sshconnect.New(sshconnect.Options{ConfigPath: config, Runner: peerMachineRunner{}, Backend: backend, InstallationMode: sshconnect.InstallPeer})
	t.Cleanup(func() { _ = service.Close() })
	token := strings.Repeat("t", 40)
	handler, err := SSHHandler(serviceSSH{service: service}, token, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	call := func(method, path string) sshCall {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept-Language", "en")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return sshCall{response.StatusCode, body}
	}
	text := i18n.New(i18n.LocaleEN)

	restarted := call(http.MethodPost, "/console/ssh/restarts/node-dev")
	var result sshconnect.InstallResult
	if restarted.status != http.StatusOK || json.Unmarshal(restarted.body, &result) != nil || result.Status != "connected" || result.NodeID != "node-dev" {
		t.Fatalf("restart = %d %s", restarted.status, restarted.body)
	}
	if records := backend.records(); len(records) != 1 || records[0].NodeID != "node-dev" || records[0].Automatic || records[0].Outcome != sshconnect.RestartRestarted {
		t.Fatalf("recorded = %#v", records)
	}
	status := call(http.MethodGet, "/console/ssh/restarts/node-dev")
	var state sshconnect.RestartState
	if status.status != http.StatusOK || json.Unmarshal(status.body, &state) != nil || state.Restart == nil || state.Restart.Status != "connected" || state.Automatic {
		t.Fatalf("restart status = %d %s", status.status, status.body)
	}

	// A restart held at its start keeps the machine: its status says it
	// runs, and an upgrade asked meanwhile is refused.
	entered, release := backend.holdNext()
	done := make(chan sshCall, 1)
	go func() { done <- call(http.MethodPost, "/console/ssh/restarts/node-dev") }()
	<-entered
	status = call(http.MethodGet, "/console/ssh/restarts/node-dev")
	if status.status != http.StatusOK || json.Unmarshal(status.body, &state) != nil || state.Restart == nil || state.Restart.Status != "installing" {
		t.Fatalf("running restart status = %d %s", status.status, status.body)
	}
	refused := call(http.MethodPost, "/console/ssh/upgrades/node-dev")
	if step := refused.step(t); refused.status != http.StatusBadRequest || step.Code != "in_progress" || step.Message != text.T(i18n.SSHRestartRunning) {
		t.Fatalf("upgrade during a restart = %d %s", refused.status, refused.body)
	}
	release()
	if finished := <-done; finished.status != http.StatusOK || json.Unmarshal(finished.body, &result) != nil || result.Status != "connected" {
		t.Fatalf("held restart = %d %s", finished.status, finished.body)
	}

	// An upgrade held at its start keeps the machine the same way.
	entered, release = backend.holdNext()
	go func() { done <- call(http.MethodPost, "/console/ssh/upgrades/node-dev") }()
	<-entered
	refused = call(http.MethodPost, "/console/ssh/restarts/node-dev")
	if step := refused.step(t); refused.status != http.StatusBadRequest || step.Code != "in_progress" || step.Message != text.T(i18n.SSHUpgradeRunning) {
		t.Fatalf("restart during an upgrade = %d %s", refused.status, refused.body)
	}
	release()
	if finished := <-done; finished.status != http.StatusOK {
		t.Fatalf("held upgrade = %d %s", finished.status, finished.body)
	}
	if records := backend.records(); len(records) != 2 {
		t.Fatalf("a refused restart was recorded: %#v", records)
	}

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		unknown := call(method, "/console/ssh/restarts/Mac%20mini")
		if step := unknown.step(t); unknown.status != http.StatusNotFound || step.Code != sshconnect.UnknownNode || !strings.Contains(step.Message, "Mac mini") {
			t.Fatalf("%s restart of an unknown node = %d %s", method, unknown.status, unknown.body)
		}
	}
}
