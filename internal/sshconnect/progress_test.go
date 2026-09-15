package sshconnect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type talkativeRunner struct {
	*recordingRunner
	installStdout, installStderr string
}

func (r *talkativeRunner) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	return &fixtureConnection{runner: r, args: append([]string{}, args...)}, nil
}

func (r *talkativeRunner) Run(ctx context.Context, args []string, input string) (Output, error) {
	if strings.Contains(input, "node_token") {
		r.mu.Lock()
		r.calls = append(r.calls, recordedCommand{args: args, stdin: input})
		r.mu.Unlock()
		if r.installStderr != "" {
			return Output{Stdout: r.installStdout, Stderr: r.installStderr}, errors.New("SSH exit")
		}
		return Output{Stdout: r.installStdout}, nil
	}
	return r.recordingRunner.Run(ctx, args, input)
}

func logText(result InstallResult) string {
	var b strings.Builder
	for _, line := range result.Log {
		b.WriteString(line.Stream + ": " + line.Text + "\n")
	}
	return b.String()
}

// While an installation runs, Status answers where it is and what the
// remote has said so far, so a person watching does not stare at a spinner.
func TestStatusReportsPhaseAndLogWhileInstalling(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	block := &blockingInstallRunner{recordingRunner: r, entered: make(chan struct{}), release: make(chan struct{})}
	svc.runner = block
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if status, err := svc.Status(plan.ID); err != nil || status.PlanID != plan.ID || status.Status != "planned" {
		t.Fatalf("status before commit = %#v %v, want planned", status, err)
	}
	var once sync.Once
	release := func() { once.Do(func() { close(block.release) }) }
	defer release()
	completed := make(chan error, 1)
	go func() { _, err := svc.Commit(t.Context(), plan.ID); completed <- err }()
	select {
	case <-block.entered:
	case err := <-completed:
		t.Fatalf("commit did not reach installer: %v", err)
	}
	status, err := svc.Status(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "installing" || status.Phase != "installation" {
		t.Fatalf("running status = %q phase %q, want installing/installation", status.Status, status.Phase)
	}
	if strings.Join(status.Phases, ",") != "preflight,registration,installation,connectivity" {
		t.Fatalf("phases = %v", status.Phases)
	}
	if text := logText(status); !strings.Contains(text, "steve: ") || !strings.Contains(text, "登记") {
		t.Fatalf("log lacks the phase narration: %q", text)
	}
	release()
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	done, err := svc.Status(plan.ID)
	if err != nil || done.Status != "connected" || done.Phase != "" {
		t.Fatalf("finished status = %#v %v", done, err)
	}
	if text := logText(done); !strings.Contains(text, "stdout: Node process started") {
		t.Fatalf("finished log lacks remote output: %q", text)
	}
	if _, err := svc.Status("nope"); err == nil {
		t.Fatal("unknown plan must not have a status")
	}
}

// A failed installation keeps the phase it failed in and the remote's
// stderr, with the node credential never written to the log.
func TestFailedInstallLogKeepsStderrAndRedactsCredential(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	svc.runner = &talkativeRunner{recordingRunner: r, installStdout: "starting with test-install-secret", installStderr: "Peer startup did not remain running; inspect ~/.steve-peer/peer.log."}
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err == nil || result.Status != "needs_attention" {
		t.Fatalf("expected failed installation: %#v %v", result, err)
	}
	if result.Phase != "installation" {
		t.Fatalf("failed phase = %q, want installation", result.Phase)
	}
	text := logText(result)
	if !strings.Contains(text, "stderr: Peer startup did not remain running") {
		t.Fatalf("log lacks remote stderr: %q", text)
	}
	if strings.Contains(text, "test-install-secret") || !strings.Contains(text, "stdout: starting with [redacted]") {
		t.Fatalf("credential leaked or stdout missing: %q", text)
	}
	status, err := svc.Status(plan.ID)
	if err != nil || status.Status != "needs_attention" || logText(status) != text {
		t.Fatalf("status after failure must carry the same log: %#v %v", status, err)
	}
}

// Output is bounded: a chatty script cannot grow the record without limit.
func TestInstallLogIsBounded(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	svc.runner = &talkativeRunner{recordingRunner: r, installStdout: strings.Repeat("line\n", 1000) + strings.Repeat("x", 5000)}
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Commit(t.Context(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Log) > maxLogLines {
		t.Fatalf("log has %d lines, cap is %d", len(result.Log), maxLogLines)
	}
	for _, line := range result.Log {
		if len(line.Text) > maxLogLineBytes {
			t.Fatalf("log line of %d bytes exceeds cap %d", len(line.Text), maxLogLineBytes)
		}
	}
}
