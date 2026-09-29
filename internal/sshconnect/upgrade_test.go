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

	"github.com/gopact-ai/steve/internal/i18n"
)

// upgradeBackend enrolls nothing; it only answers where a machine is and
// notices when the service says the program was swapped.
type upgradeBackend struct {
	fakeBackend
	alias       string
	upgraded    []string
	upgradedErr error
	binaryPath  string
	// unknown is a node ID no machine of the backend has.
	unknown string
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

func (b *upgradeBackend) Knows(_ context.Context, nodeID string) bool {
	return nodeID != b.unknown
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

// A node ID that names no machine, such as a machine's display name, is
// refused as unknown: no upgrade record is kept, nothing is run, and there
// is no status to read for it. A known machine this node cannot reach keeps
// its record, so why it was refused stays readable.
func TestUpgradeRefusesAnUnknownNodeWithoutKeepingARecord(t *testing.T) {
	svc, runner, backend, _ := upgradeFixture(t)
	backend.unknown = "Mac mini"
	result, err := svc.Upgrade(t.Context(), "Mac mini")
	var step *StepError
	if !errors.As(err, &step) || step.Code != "unknown_node" || result.Status != "" || result.PlanID != "" {
		t.Fatalf("unknown node upgrade = %#v %v", result, err)
	}
	if !strings.Contains(step.Message, "Mac mini") {
		t.Fatalf("the refusal does not name what was asked for: %q", step.Message)
	}
	if _, err := svc.UpgradeStatus(t.Context(), "Mac mini"); !errors.As(err, &step) || step.Code != "unknown_node" {
		t.Fatalf("unknown node status = %v", err)
	}
	svc.mu.Lock()
	kept := len(svc.plans) + len(svc.upgrades)
	svc.mu.Unlock()
	if kept != 0 || len(runner.calls) != 0 {
		t.Fatalf("an unknown node left %d records and ran %d commands", kept, len(runner.calls))
	}
	backend.alias = ""
	if result, err := svc.Upgrade(t.Context(), "node-1"); !errors.As(err, &step) || step.Code != "upgrade_target" || result.Status != "needs_attention" {
		t.Fatalf("known machine without a tunnel = %#v %v", result, err)
	}
	if status, err := svc.UpgradeStatus(t.Context(), "node-1"); err != nil || status.Status != "needs_attention" {
		t.Fatalf("known machine's refused upgrade is not readable: %#v %v", status, err)
	}
}

// The refusal says what this node does not know, not why: a well-formed
// node ID can be unknown here too, for a machine that joined while this
// node's replica could not catch up, so it neither blames a display name
// nor speaks for the whole cluster. Chinese names the ID the way the
// console labels it.
func TestUnknownNodeRefusalSaysThisNodeDoesNotKnowTheID(t *testing.T) {
	svc, _, backend, _ := upgradeFixture(t)
	backend.unknown = "node-7f3a"
	refusal := func(locale i18n.Locale) *StepError {
		t.Helper()
		_, err := svc.Upgrade(i18n.WithLocale(t.Context(), locale), "node-7f3a")
		var step *StepError
		if !errors.As(err, &step) || step.Code != "unknown_node" {
			t.Fatalf("%s: unknown node upgrade = %v", locale, err)
		}
		return step
	}
	zh, en := refusal(i18n.LocaleZH), refusal(i18n.LocaleEN)
	for _, want := range []string{"node-7f3a", "本机不知道", "节点 ID"} {
		if !strings.Contains(zh.Message, want) {
			t.Errorf("zh refusal %q does not say %q", zh.Message, want)
		}
	}
	for _, want := range []string{"node-7f3a", "does not know"} {
		if !strings.Contains(en.Message, want) {
			t.Errorf("en refusal %q does not say %q", en.Message, want)
		}
	}
	for _, said := range []string{zh.Message, zh.Suggestion, en.Message, en.Suggestion} {
		for _, presumed := range []string{"集群里没有", "显示名", "display name", "No machine in the cluster"} {
			if strings.Contains(said, presumed) {
				t.Errorf("refusal %q presumes %q", said, presumed)
			}
		}
	}
}

// A known machine with no upgrade running or just finished has no status
// to read, whether it was never upgraded from here or the record of its
// last upgrade expired: both are answered alike, in the language asked
// for. A backend that upgrades nothing has no upgrade of any machine.
func TestUpgradeStatusWithoutARecordSaysTheMachineHasNoUpgrade(t *testing.T) {
	svc, _, _, _ := upgradeFixture(t)
	svc.ttl = 50 * time.Millisecond
	none := func(svc *Service, node, when string) {
		t.Helper()
		for _, tc := range []struct {
			locale i18n.Locale
			says   string
		}{{i18n.LocaleZH, "没有进行中或刚结束的升级"}, {i18n.LocaleEN, "no upgrade running or just finished"}} {
			_, err := svc.UpgradeStatus(i18n.WithLocale(t.Context(), tc.locale), node)
			var step *StepError
			if !errors.As(err, &step) {
				t.Errorf("%s, %s: status of %s = %v", when, tc.locale, node, err)
				continue
			}
			if step.Code != "unknown_upgrade" || !strings.Contains(step.Message, tc.says) {
				t.Errorf("%s, %s: status of %s refused as %s: %q", when, tc.locale, node, step.Code, step.Message)
			}
		}
	}
	none(svc, "node-1", "never upgraded")
	if _, err := svc.Upgrade(t.Context(), "node-1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := svc.UpgradeStatus(t.Context(), "node-1"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the finished upgrade's record never expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	none(svc, "node-1", "record expired")
	unsupported := New(Options{Backend: &fakeBackend{}})
	t.Cleanup(func() { _ = unsupported.Close() })
	none(unsupported, "node-1", "no upgrade backend")
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
// while its status reads how the first is going; the record of a finished
// upgrade goes away like a plan does.
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
	status, err := svc.UpgradeStatus(t.Context(), "node-1")
	if err != nil || status.Status != "installing" || status.PlanID == "" || len(status.Phases) != 4 {
		t.Fatalf("status while upgrading = %#v %v", status, err)
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
