package sshconnect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

// restartBackend knows two machines, node-1 over alias dev and node-2 over
// dev2, and upgrades them as upgradeBackend does. It notices when the
// service says a machine's peer was started and keeps what it was asked
// to record.
type restartBackend struct {
	upgradeBackend
	mu sync.Mutex
	// aliases is the alias of each machine's link; a machine missing here
	// has no link.
	aliases      map[string]string
	restarted    []string
	restartedErr error
	// restartHold keeps Restarted from returning until closed or its
	// context ends; restartHolding counts the calls that reached it.
	restartHold    chan struct{}
	restartHolding atomic.Int32
	records        []RestartRecord
	// watched are the machines automatic start looks after; answering
	// ones answer the cluster and unreachable ones have no SSH session.
	watched     []string
	answering   map[string]bool
	unreachable map[string]bool
}

func (b *restartBackend) RestartTarget(_ context.Context, nodeID string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	alias, ok := b.aliases[nodeID]
	if !ok {
		return "", errors.New("这台机器没有记录 SSH 隧道")
	}
	return alias, nil
}

func (b *restartBackend) Restarted(ctx context.Context, nodeID string) error {
	Report(ctx, "机器已回到集群")
	b.mu.Lock()
	hold := b.restartHold
	b.mu.Unlock()
	if hold != nil {
		b.restartHolding.Add(1)
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.restarted = append(b.restarted, nodeID)
	return b.restartedErr
}

func (b *restartBackend) RecordRestart(_ context.Context, record RestartRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records = append(b.records, record)
	return nil
}

func (b *restartBackend) Watched(context.Context) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.watched)
}

func (b *restartBackend) Answers(_ context.Context, nodeID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.answering[nodeID]
}

func (b *restartBackend) Reachable(_ context.Context, nodeID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.unreachable[nodeID]
}

func (b *restartBackend) set(change func(*restartBackend)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	change(b)
}

func (b *restartBackend) restartedNodes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.restarted)
}

func (b *restartBackend) recorded() []RestartRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.records)
}

type restartScript struct{ alias, script string }

// restartRunner answers the restart script through answer, outside the
// recording runner's lock, so a test can hold one restart while others
// run; everything else goes to the recording runner.
type restartRunner struct {
	*recordingRunner
	mu      sync.Mutex
	answer  func(ctx context.Context, script string) (Output, error)
	scripts []restartScript
}

func (r *restartRunner) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	return &fixtureConnection{runner: r, args: append([]string{}, args...)}, nil
}

func (r *restartRunner) Run(ctx context.Context, args []string, input string) (Output, error) {
	if !strings.Contains(input, "STEVE_RESTART") {
		return r.recordingRunner.Run(ctx, args, input)
	}
	r.mu.Lock()
	r.scripts = append(r.scripts, restartScript{alias: args[len(args)-2], script: input})
	answer := r.answer
	r.mu.Unlock()
	if answer == nil {
		return peerRestarted(ctx, input)
	}
	return answer(ctx, input)
}

func (r *restartRunner) answerWith(answer func(context.Context, string) (Output, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer = answer
}

func (r *restartRunner) restarts() []restartScript {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.scripts)
}

func peerRestarted(context.Context, string) (Output, error) {
	return Output{Stdout: "Stopping peer process 4242.\nPeer restarted; the cluster still has to see it come back.\nSTEVE_RESTART\trestarted\n"}, nil
}

func peerStarted(context.Context, string) (Output, error) {
	return Output{Stdout: "No peer was running; peer started. The cluster still has to see it come back.\nSTEVE_RESTART\tstarted\n"}, nil
}

func peerStillRunning(context.Context, string) (Output, error) {
	return Output{Stdout: "Peer process 4242 is still running; it is left alone.\nSTEVE_RESTART\trunning\n"}, nil
}

// peerExits is a restart script ending with that exit status after saying
// what the script says when the started peer does not stay up.
func peerExits(code int) func(context.Context, string) (Output, error) {
	return func(context.Context, string) (Output, error) {
		return Output{Stderr: "The peer did not stay running; the peer is down. Last lines of ~/.steve-peer/peer.log:\nconfig.json: cluster identity is unreadable\n"}, exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	}
}

func restartFixture(t *testing.T) (*Service, *restartRunner, *restartBackend) {
	t.Helper()
	path := configFixture(t, map[string]string{"config": "Host dev\nHostName dev.example\nHost dev2\nHostName dev2.example\n"})
	raw := make([]byte, 64)
	copy(raw, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	raw[16], raw[18], raw[20], raw[52] = 2, 62, 1, 64
	binary := filepath.Join(t.TempDir(), "steve")
	if err := os.WriteFile(binary, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &restartRunner{recordingRunner: &recordingRunner{checkOutput: peerProbe}}
	backend := &restartBackend{upgradeBackend: upgradeBackend{alias: "dev", binaryPath: binary}, aliases: map[string]string{"node-1": "dev", "node-2": "dev2"}}
	svc := New(Options{ConfigPath: path, Runner: runner, Backend: backend, InstallationMode: InstallPeer})
	t.Cleanup(func() { _ = svc.Close() })
	return svc, runner, backend
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func stepCode(err error) string {
	var step *StepError
	if errors.As(err, &step) {
		return step.Code
	}
	return ""
}

var restartPhases = []string{PhasePreflight, PhaseRestart, PhaseConnectivity}

// A restart reaches the machine over the alias of its link and runs the
// restart script there: the program it has is started again, nothing is
// uploaded and no program is moved. The backend then confirms the machine
// is back; the restart's status reads by node ID while it runs and after,
// and the restart is recorded as manual and what it did.
func TestRestartRestartsThePeerAndConfirmsTheMachineIsBack(t *testing.T) {
	svc, runner, backend := restartFixture(t)
	result, err := svc.Restart(t.Context(), "node-1")
	if err != nil || !result.Connected || result.Status != "connected" || result.Phase != "" || !slices.Equal(result.Phases, restartPhases) {
		t.Fatalf("restart = %#v %v", result, err)
	}
	scripts := runner.restarts()
	if len(scripts) != 1 || scripts[0].alias != "dev" || scripts[0].script != nodebootstrap.BuildPeerRestart(nodebootstrap.RestartSpec{}) {
		t.Fatalf("expected the restart script over dev once, got %d runs", len(scripts))
	}
	for _, call := range runner.calls {
		if call.upload || strings.Contains(call.stdin, "steve.previous") {
			t.Fatal("a restart uploaded or swapped a program")
		}
	}
	if restarted := backend.restartedNodes(); !slices.Equal(restarted, []string{"node-1"}) {
		t.Fatalf("the backend was asked to confirm %v", restarted)
	}
	log := logText(result)
	if !strings.Contains(log, "Stopping peer process 4242") || !strings.Contains(log, "机器已回到集群") || strings.Contains(log, "STEVE_RESTART") {
		t.Fatalf("the log does not say what the script and the backend did:\n%s", log)
	}
	state, err := svc.RestartStatus(t.Context(), "node-1")
	if err != nil || state.Restart == nil || state.Restart.PlanID != result.PlanID || state.Restart.Status != "connected" || state.Automatic {
		t.Fatalf("status = %#v %v", state, err)
	}
	if other, err := svc.RestartStatus(t.Context(), "node-2"); err != nil || other.Restart != nil {
		t.Fatalf("a machine never restarted has a restart: %#v %v", other, err)
	}
	records := backend.recorded()
	if len(records) != 1 || records[0].NodeID != "node-1" || records[0].Automatic || records[0].Outcome != RestartRestarted || records[0].Reason != "" || records[0].At.IsZero() {
		t.Fatalf("records = %#v", records)
	}
}

// A machine whose peer was not running gets it started, and says so
// rather than that it was restarted.
func TestRestartSaysAPeerThatWasNotRunningWasStarted(t *testing.T) {
	svc, runner, backend := restartFixture(t)
	runner.answerWith(peerStarted)
	en := i18n.New(i18n.LocaleEN)
	result, err := svc.Restart(i18n.WithLocale(t.Context(), i18n.LocaleEN), "node-1")
	if err != nil || !result.Connected {
		t.Fatalf("restart = %#v %v", result, err)
	}
	if last := result.Steps[len(result.Steps)-1]; last.Message != en.T(i18n.SSHStarted) {
		t.Fatalf("the restart ended with %q", last.Message)
	}
	if records := backend.recorded(); len(records) != 1 || records[0].Outcome != RestartStarted {
		t.Fatalf("records = %#v", records)
	}
}

// What the script found on the machine is what the owner is told: a peer
// that did not stay up, with the lines its log ended on; no peer
// installation; another installation holding the machine; or a script
// that ended some other way. None of them waits for the machine to come
// back, and each is recorded as a failed restart with its reason.
func TestRestartTellsWhatTheScriptFoundOnTheMachine(t *testing.T) {
	for exit, code := range map[int]string{28: "restart_down", 30: "peer_missing", 21: "restart_busy", 1: "restart_uncertain", 0: "restart_uncertain"} {
		svc, runner, backend := restartFixture(t)
		if exit == 0 {
			runner.answerWith(func(context.Context, string) (Output, error) { return Output{Stdout: "no verdict"}, nil })
		} else {
			runner.answerWith(peerExits(exit))
		}
		result, err := svc.Restart(t.Context(), "node-1")
		var step *StepError
		if !errors.As(err, &step) || step.Code != code || result.Connected || result.Status != "needs_attention" || result.Phase != PhaseRestart {
			t.Fatalf("exit %d: %#v %v", exit, result, err)
		}
		if exit == 28 && !strings.Contains(logText(result), "config.json: cluster identity is unreadable") {
			t.Fatalf("exit 28: the peer's last log lines are missing:\n%s", logText(result))
		}
		if restarted := backend.restartedNodes(); len(restarted) != 0 {
			t.Fatalf("exit %d: the backend was asked to confirm %v", exit, restarted)
		}
		if records := backend.recorded(); len(records) != 1 || records[0].Outcome != RestartFailed || records[0].Reason != step.Message {
			t.Fatalf("exit %d: records = %#v", exit, records)
		}
	}
}

// A machine that does not come back after its peer was started is
// reported as such, with the backend's reason.
func TestRestartReportsAMachineThatDoesNotComeBack(t *testing.T) {
	svc, _, backend := restartFixture(t)
	backend.restartedErr = errors.New("3 分钟内没有应答")
	result, err := svc.Restart(t.Context(), "node-1")
	var step *StepError
	if !errors.As(err, &step) || step.Code != "restart_unconfirmed" || result.Connected || result.Phase != PhaseConnectivity {
		t.Fatalf("unconfirmed: %#v %v", result, err)
	}
	if !strings.Contains(step.Message, "3 分钟内没有应答") {
		t.Fatalf("the backend's reason is not carried to the owner: %q", step.Message)
	}
	if records := backend.recorded(); len(records) != 1 || records[0].Outcome != RestartFailed {
		t.Fatalf("records = %#v", records)
	}
}

// A machine without a link here, or without a peer installation, is
// refused before anything runs on it; the refusal is readable afterwards
// and recorded like any failed restart.
func TestRestartRefusesMachinesItCannotReachOrThatHaveNoPeer(t *testing.T) {
	svc, runner, backend := restartFixture(t)
	backend.set(func(b *restartBackend) { b.aliases = nil })
	result, err := svc.Restart(t.Context(), "node-1")
	if stepCode(err) != "restart_target" || result.Phase != PhasePreflight || result.Status != "needs_attention" {
		t.Fatalf("no link: %#v %v", result, err)
	}
	if state, err := svc.RestartStatus(t.Context(), "node-1"); err != nil || state.Restart == nil || state.Restart.Status != "needs_attention" {
		t.Fatalf("the refused restart is not readable: %#v %v", state, err)
	}
	backend.set(func(b *restartBackend) { b.aliases = map[string]string{"node-1": "dev"} })
	runner.checkOutput = completeProbe
	if _, err := svc.Restart(t.Context(), "node-1"); stepCode(err) != "peer_missing" {
		t.Fatalf("no peer: %v", err)
	}
	if scripts := runner.restarts(); len(scripts) != 0 {
		t.Fatalf("a refused restart ran the script %d times", len(scripts))
	}
	if records := backend.recorded(); len(records) != 2 || records[0].Outcome != RestartFailed || records[1].Outcome != RestartFailed {
		t.Fatalf("records = %#v", records)
	}
}

// A node ID no machine has is refused as an upgrade refuses it: nothing
// runs, nothing is recorded and no status is kept for it.
func TestRestartRefusesAnUnknownNodeWithoutKeepingARecord(t *testing.T) {
	svc, runner, backend := restartFixture(t)
	backend.unknown = "Mac mini"
	result, err := svc.Restart(t.Context(), "Mac mini")
	var step *StepError
	if !errors.As(err, &step) || step.Code != UnknownNode || result.Status != "" || result.PlanID != "" || !strings.Contains(step.Message, "Mac mini") {
		t.Fatalf("unknown node restart = %#v %v", result, err)
	}
	if _, err := svc.RestartStatus(t.Context(), "Mac mini"); stepCode(err) != UnknownNode {
		t.Fatalf("unknown node status = %v", err)
	}
	svc.mu.Lock()
	kept := len(svc.plans) + len(svc.restarts)
	svc.mu.Unlock()
	if kept != 0 || len(runner.restarts()) != 0 || len(runner.calls) != 0 || len(backend.recorded()) != 0 {
		t.Fatalf("an unknown node left %d records, ran %d commands and recorded %d restarts", kept, len(runner.calls), len(backend.recorded()))
	}
}

// A backend that restarts no machine refuses a restart and the status of
// one alike, whatever the node ID, in the language asked for.
func TestABackendThatRestartsNothingRefusesARestartAndItsStatusAlike(t *testing.T) {
	svc := New(Options{Backend: &fakeBackend{}})
	t.Cleanup(func() { _ = svc.Close() })
	said := func(step *StepError) [5]string {
		return [5]string{step.Stage, step.Code, step.Message, step.Suggestion, step.Error()}
	}
	for _, locale := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		ctx := i18n.WithLocale(t.Context(), locale)
		for _, node := range []string{"node-1", "Mac mini"} {
			_, restartErr := svc.Restart(ctx, node)
			_, statusErr := svc.RestartStatus(ctx, node)
			var restart, status *StepError
			if !errors.As(restartErr, &restart) || !errors.As(statusErr, &status) {
				t.Fatalf("%s, %s: restart = %v, status = %v", locale, node, restartErr, statusErr)
			}
			if restart.Code != "restart_unsupported" || restart.Message != i18n.New(locale).T(i18n.SSHRestartUnsupported) {
				t.Errorf("%s, %s: restart refused as %s: %q", locale, node, restart.Code, restart.Message)
			}
			if said(status) != said(restart) {
				t.Errorf("%s, %s: status refused as %q, restart as %q", locale, node, said(status), said(restart))
			}
		}
	}
	svc.mu.Lock()
	kept := len(svc.plans) + len(svc.restarts)
	svc.mu.Unlock()
	if kept != 0 {
		t.Fatalf("refusals left %d records", kept)
	}
}

// One machine runs one upgrade or one restart at a time. A restart is
// refused while the machine is being upgraded, and an upgrade or a second
// restart while it is being restarted; another machine is not held up.
// A refusal replaces no record and runs nothing.
func TestRestartAndUpgradeOfAMachineExcludeEachOther(t *testing.T) {
	svc, runner, backend := restartFixture(t)
	zh := i18n.New(i18n.LocaleZH)
	refusedAs := func(err error, key i18n.Key) bool {
		var step *StepError
		return errors.As(err, &step) && step.Code == "in_progress" && step.Message == zh.T(key)
	}
	ctx := i18n.WithLocale(t.Context(), i18n.LocaleZH)

	backend.hold = make(chan struct{})
	upgraded := make(chan error, 1)
	go func() { _, err := svc.Upgrade(context.Background(), "node-1"); upgraded <- err }()
	waitUntil(t, "the upgrade never reached the backend", func() bool { return backend.holding.Load() == 1 })
	if result, err := svc.Restart(ctx, "node-1"); !refusedAs(err, i18n.SSHUpgradeRunning) || result.Status != "" {
		t.Fatalf("restart during an upgrade = %#v %v", result, err)
	}
	if _, err := svc.Restart(ctx, "node-2"); err != nil {
		t.Fatalf("another machine's restart was held up: %v", err)
	}
	close(backend.hold)
	if err := <-upgraded; err != nil {
		t.Fatal(err)
	}
	upgrade, err := svc.UpgradeStatus(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}

	backend.set(func(b *restartBackend) { b.restartHold = make(chan struct{}) })
	restarted := make(chan error, 1)
	go func() { _, err := svc.Restart(context.Background(), "node-1"); restarted <- err }()
	waitUntil(t, "the restart never reached the backend", func() bool { return backend.restartHolding.Load() == 1 })
	state, err := svc.RestartStatus(ctx, "node-1")
	if err != nil || state.Restart == nil || state.Restart.Status != "installing" || state.Restart.Phase != PhaseConnectivity || !slices.Equal(state.Restart.Phases, restartPhases) {
		t.Fatalf("status while restarting = %#v %v", state, err)
	}
	if result, err := svc.Upgrade(ctx, "node-1"); !refusedAs(err, i18n.SSHRestartRunning) || result.Status != "" {
		t.Fatalf("upgrade during a restart = %#v %v", result, err)
	}
	if result, err := svc.Restart(ctx, "node-1"); !refusedAs(err, i18n.SSHRestartRunning) || result.Status != "" {
		t.Fatalf("second restart = %#v %v", result, err)
	}
	backend.set(func(b *restartBackend) { close(b.restartHold); b.restartHold = nil })
	if err := <-restarted; err != nil {
		t.Fatal(err)
	}
	if after, err := svc.UpgradeStatus(ctx, "node-1"); err != nil || after.PlanID != upgrade.PlanID {
		t.Fatalf("the refused upgrade replaced the machine's upgrade record: %#v %v", after, err)
	}
	var dev int
	for _, script := range runner.restarts() {
		if script.alias == "dev" {
			dev++
		}
	}
	if dev != 1 || len(backend.recorded()) != 2 {
		t.Fatalf("node-1 ran the restart script %d times; %d restarts were recorded", dev, len(backend.recorded()))
	}
}

// A machine never restarted from here has no restart to show, and a
// finished restart's record goes away like an upgrade's.
func TestRestartStatusWithoutARecordHasNoRestart(t *testing.T) {
	svc, _, _ := restartFixture(t)
	svc.ttl = 50 * time.Millisecond
	if state, err := svc.RestartStatus(t.Context(), "node-1"); err != nil || state.Restart != nil || state.AutoStart != nil {
		t.Fatalf("status of a machine never restarted = %#v %v", state, err)
	}
	if _, err := svc.Restart(t.Context(), "node-1"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the finished restart's record never expired", func() bool {
		state, err := svc.RestartStatus(t.Context(), "node-1")
		return err == nil && state.Restart == nil
	})
}
