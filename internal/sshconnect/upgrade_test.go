package sshconnect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// upgradeBackend enrolls nothing; it only answers where a machine is and
// notices when the service says the program was swapped.
type upgradeBackend struct {
	fakeBackend
	alias       string
	upgraded    []string
	upgradedErr error
	binaryPath  string
	// hold keeps Upgraded from returning until closed; holding counts the
	// calls waiting there.
	hold    chan struct{}
	holding atomic.Int32
}

func (b *upgradeBackend) UpgradeTarget(_ context.Context, nodeID string) (UpgradeTarget, error) {
	if b.alias == "" {
		return UpgradeTarget{}, errors.New("这台机器没有记录 SSH 隧道")
	}
	return UpgradeTarget{Alias: b.alias, Version: "abc1234", FindBinary: func(platform string) (string, bool) { return b.binaryPath, platform == "linux/amd64" }}, nil
}

func (b *upgradeBackend) Upgraded(ctx context.Context, nodeID string) error {
	Report(ctx, "隧道已恢复")
	if b.hold != nil {
		b.holding.Add(1)
		<-b.hold
	}
	b.upgraded = append(b.upgraded, nodeID)
	return b.upgradedErr
}

var peerProbe = strings.Replace(completeProbe, "STEVE_CHECK\texisting\t0\n", "STEVE_CHECK\texisting_path_peer_state\t~/.steve-peer\nSTEVE_CHECK\texisting\t1\n", 1)

func upgradeFixture(t *testing.T) (*Service, *recordingRunner, *upgradeBackend, []byte) {
	t.Helper()
	path := configFixture(t, map[string]string{"config": "Host dev\nHostName dev.example\n"})
	raw := make([]byte, 64)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	raw[16], raw[18], raw[20], raw[52] = 2, 62, 1, 64
	binary := filepath.Join(t.TempDir(), "steve")
	if err := os.WriteFile(binary, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runner, backend := &recordingRunner{checkOutput: peerProbe}, &upgradeBackend{alias: "dev", binaryPath: binary}
	svc := New(Options{ConfigPath: path, Runner: runner, Backend: backend, InstallationMode: InstallPeer})
	t.Cleanup(func() { _ = svc.Close() })
	return svc, runner, backend, raw
}

// An upgrade sends this build's program over the machine's alias, swaps it
// in with the upgrade script and lets the backend confirm the machine came
// back; its status is readable by node ID while it runs and afterwards.
func TestUpgradeSendsTheProgramSwapsItAndConfirmsTheMachineIsBack(t *testing.T) {
	svc, runner, backend, raw := upgradeFixture(t)
	result, err := svc.Upgrade(t.Context(), "node-1")
	if err != nil || !result.Connected || result.Status != "connected" {
		t.Fatalf("upgrade = %#v %v", result, err)
	}
	uploads, swaps := 0, 0
	for _, call := range runner.calls {
		if call.upload {
			uploads++
			if call.stdin != string(raw) {
				t.Fatal("uploaded bytes differ from the program")
			}
		}
		if strings.Contains(call.stdin, "steve.previous") {
			swaps++
			if !strings.Contains(call.stdin, result.PlanID) {
				t.Fatal("the upgrade script does not name the upload it verifies")
			}
		}
	}
	if uploads != 1 || swaps != 1 || len(backend.upgraded) != 1 || backend.upgraded[0] != "node-1" {
		t.Fatalf("uploads=%d swaps=%d upgraded=%v", uploads, swaps, backend.upgraded)
	}
	status, err := svc.UpgradeStatus(t.Context(), "node-1")
	if err != nil || status.PlanID != result.PlanID || status.Phase != "" || len(status.Phases) != 4 {
		t.Fatalf("status = %#v %v", status, err)
	}
	var narrated bool
	for _, line := range status.Log {
		narrated = narrated || strings.Contains(line.Text, "隧道已恢复")
	}
	if !narrated {
		t.Fatal("what the backend reported while confirming is missing from the log")
	}
	if _, err := svc.UpgradeStatus(t.Context(), "node-2"); err == nil {
		t.Fatal("a machine that was never upgraded has no status")
	}
}

// A machine without a recorded tunnel, or without a peer installation,
// is refused before anything is sent.
func TestUpgradeRefusesMachinesItCannotReachOrThatHaveNoPeer(t *testing.T) {
	svc, runner, backend, _ := upgradeFixture(t)
	backend.alias = ""
	result, err := svc.Upgrade(t.Context(), "node-1")
	var step *StepError
	if !errors.As(err, &step) || step.Code != "upgrade_target" || result.Phase != PhasePreflight {
		t.Fatalf("missing alias: %#v %v", result, err)
	}
	backend.alias = "dev"
	runner.checkOutput = completeProbe
	result, err = svc.Upgrade(t.Context(), "node-1")
	if !errors.As(err, &step) || step.Code != "peer_missing" {
		t.Fatalf("missing peer: %#v %v", result, err)
	}
	for _, call := range runner.calls {
		if call.upload || strings.Contains(call.stdin, "steve.previous") {
			t.Fatal("a refused upgrade sent or swapped the program")
		}
	}
}

// When the machine does not come back on the new build the upgrade is
// reported as unconfirmed, with the program already swapped.
func TestUpgradeReportsAnUnconfirmedReturn(t *testing.T) {
	svc, _, backend, _ := upgradeFixture(t)
	backend.upgradedErr = errors.New("版本仍为 5035948")
	result, err := svc.Upgrade(t.Context(), "node-1")
	var step *StepError
	if !errors.As(err, &step) || step.Code != "upgrade_unconfirmed" || result.Connected || result.Phase != PhaseConnectivity {
		t.Fatalf("unconfirmed: %#v %v", result, err)
	}
	if !strings.Contains(step.Message, "5035948") {
		t.Fatal("the backend's reason is not carried to the owner")
	}
}

// The script tells a rollback apart from a peer it could not bring back;
// the owner is told which of the two the machine is in.
func TestUpgradeTellsARollbackFromAPeerLeftDown(t *testing.T) {
	for exit, code := range map[int]string{26: "upgrade_rejected", 28: "upgrade_down", 22: "upgrade_uncertain"} {
		svc, runner, backend, _ := upgradeFixture(t)
		runner.swapExit = exit
		result, err := svc.Upgrade(t.Context(), "node-1")
		var step *StepError
		if !errors.As(err, &step) || step.Code != code || result.Connected || len(backend.upgraded) != 0 {
			t.Fatalf("exit %d: %#v %v", exit, result, err)
		}
	}
}

// A second upgrade of a machine still being upgraded is refused outright,
// and the record of a finished upgrade goes away like a plan does.
func TestUpgradeRunsOncePerMachineAndItsRecordExpires(t *testing.T) {
	svc, runner, backend, _ := upgradeFixture(t)
	svc.ttl = 50 * time.Millisecond
	release := make(chan struct{})
	backend.hold = release
	first := make(chan error, 1)
	go func() { _, err := svc.Upgrade(context.Background(), "node-1"); first <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for backend.holding.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first upgrade never reached the backend")
		}
		time.Sleep(5 * time.Millisecond)
	}
	result, err := svc.Upgrade(t.Context(), "node-1")
	var step *StepError
	if !errors.As(err, &step) || step.Code != "in_progress" || result.Status != "" {
		t.Fatalf("second upgrade = %#v %v", result, err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpgradeStatus(t.Context(), "node-1"); err != nil {
		t.Fatalf("a finished upgrade is readable: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, err := svc.UpgradeStatus(t.Context(), "node-1"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the finished upgrade's record never expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls := len(runner.calls); calls == 0 {
		t.Fatal("nothing ran")
	}
}
