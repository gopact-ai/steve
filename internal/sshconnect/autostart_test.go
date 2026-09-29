package sshconnect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// autoStartFixture watches node-1, which does not answer the cluster, with
// a clock that only moves when the test moves it. A peer started on it
// stays up.
func autoStartFixture(t *testing.T) (*Service, *restartRunner, *restartBackend, *fakeClock) {
	t.Helper()
	svc, runner, backend := restartFixture(t)
	clock := &fakeClock{at: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)}
	svc.now = clock.Now
	runner.answerWith(peerStarted)
	backend.set(func(b *restartBackend) { b.watched = []string{"node-1"} })
	return svc, runner, backend, clock
}

// sweepOnce looks at every watched machine once and waits for the
// attempts that look started to settle.
func sweepOnce(svc *Service) {
	svc.sweep()
	svc.autoRuns.Wait()
}

func autoState(t *testing.T, svc *Service, nodeID string) AutoStartState {
	t.Helper()
	state, err := svc.RestartStatus(t.Context(), nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.AutoStart == nil {
		t.Fatalf("%s has no automatic start state", nodeID)
	}
	return *state.AutoStart
}

var startIfStopped = nodebootstrap.BuildPeerRestart(nodebootstrap.RestartSpec{IfStopped: true})

// A machine that has not answered the cluster for a minute, whose SSH
// session is up, gets its peer started with the script that starts one
// only where none runs; the backend confirms it is back and the start is
// recorded as automatic.
func TestAutoStartStartsAMachineOfflinePastTheThresholdWhosePeerIsDown(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	sweepOnce(svc)
	if state := autoState(t, svc, "node-1"); state.State != "waiting" || !state.OfflineSince.Equal(clock.Now()) || state.Attempts != 0 || state.Limit != autoStartLimit {
		t.Fatalf("a machine that just went offline: %#v", state)
	}
	clock.Advance(autoStartAfter - time.Second)
	sweepOnce(svc)
	if scripts := runner.restarts(); len(scripts) != 0 {
		t.Fatalf("a machine offline for less than %s was started", autoStartAfter)
	}
	clock.Advance(time.Second)
	sweepOnce(svc)
	scripts := runner.restarts()
	if len(scripts) != 1 || scripts[0].alias != "dev" || scripts[0].script != startIfStopped {
		t.Fatalf("expected the start-if-stopped script over dev once, got %d runs", len(scripts))
	}
	if restarted := backend.restartedNodes(); len(restarted) != 1 || restarted[0] != "node-1" {
		t.Fatalf("the backend was asked to confirm %v", restarted)
	}
	state, err := svc.RestartStatus(t.Context(), "node-1")
	if err != nil || state.Restart == nil || state.Restart.Status != "connected" || !state.Automatic || state.AutoStart == nil || state.AutoStart.State != "watching" || state.AutoStart.Attempts != 1 {
		t.Fatalf("after the start: %#v %v", state, err)
	}
	records := backend.recorded()
	if len(records) != 1 || records[0].NodeID != "node-1" || !records[0].Automatic || records[0].Outcome != RestartStarted {
		t.Fatalf("records = %#v", records)
	}
}

// A peer process that still runs is never ended by automatic start, only
// reported: the machine shows it, nothing waits for the machine to come
// back, and the machine is looked at again only after a while. Finding it
// running again is not recorded a second time.
func TestAutoStartLeavesARunningPeerAlone(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	runner.answerWith(peerStillRunning)
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	state := autoState(t, svc, "node-1")
	if state.State != "peer_running" || state.Attempts != 0 || state.Reason == "" {
		t.Fatalf("a running peer: %#v", state)
	}
	if restarted := backend.restartedNodes(); len(restarted) != 0 {
		t.Fatalf("the backend was asked to confirm %v", restarted)
	}
	clock.Advance(autoStartRecheck - time.Second)
	sweepOnce(svc)
	if scripts := runner.restarts(); len(scripts) != 1 {
		t.Fatalf("a running peer was looked at again %d times within %s", len(scripts)-1, autoStartRecheck)
	}
	clock.Advance(time.Second)
	sweepOnce(svc)
	scripts := runner.restarts()
	if len(scripts) != 2 {
		t.Fatalf("a running peer was not looked at again after %s", autoStartRecheck)
	}
	for _, script := range scripts {
		if script.script != startIfStopped {
			t.Fatal("automatic start ran a script that stops a running peer")
		}
	}
	if records := backend.recorded(); len(records) != 1 || records[0].Outcome != RestartRunning || !records[0].Automatic {
		t.Fatalf("records = %#v", records)
	}
}

// Failed starts are retried after 1, 2, 4 and 8 minutes; the fifth
// failure stops automatic start for the machine, which then shows the
// last reason. Each failure is recorded, the last as the one that stopped
// it, and nothing is tried afterwards however long the machine stays down.
func TestAutoStartBacksOffAndStopsAfterItsLimit(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	runner.answerWith(peerExits(28))
	zh := i18n.New(i18n.LocaleZH)
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	for attempt, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute} {
		state := autoState(t, svc, "node-1")
		if state.State != "retrying" || state.Attempts != attempt+1 || !state.NextAt.Equal(clock.Now().Add(wait)) || state.LastError != zh.T(i18n.SSHRestartDown) {
			t.Fatalf("after failure %d: %#v", attempt+1, state)
		}
		clock.Advance(wait - time.Second)
		sweepOnce(svc)
		if scripts := runner.restarts(); len(scripts) != attempt+1 {
			t.Fatalf("failure %d was retried within %s", attempt+1, wait)
		}
		clock.Advance(time.Second)
		sweepOnce(svc)
		if scripts := runner.restarts(); len(scripts) != attempt+2 {
			t.Fatalf("failure %d was not retried after %s", attempt+1, wait)
		}
	}
	state := autoState(t, svc, "node-1")
	if state.State != "stopped" || state.Attempts != autoStartLimit || state.LastError != zh.T(i18n.SSHRestartDown) {
		t.Fatalf("after the last failure: %#v", state)
	}
	clock.Advance(24 * time.Hour)
	sweepOnce(svc)
	if scripts := runner.restarts(); len(scripts) != autoStartLimit {
		t.Fatalf("automatic start went on after it stopped: %d runs", len(scripts))
	}
	records := backend.recorded()
	if len(records) != autoStartLimit {
		t.Fatalf("%d attempts recorded", len(records))
	}
	for i, record := range records {
		want := RestartFailed
		if i == autoStartLimit-1 {
			want = RestartStopped
		}
		if !record.Automatic || record.Outcome != want || record.Reason != zh.T(i18n.SSHRestartDown) {
			t.Fatalf("record %d = %#v", i, record)
		}
	}
}

// A machine that leaves the cluster is dropped at once: the start running
// for it is cut off and nothing is recorded or tried for it again.
func TestAutoStartStopsForAMachineThatWasRemoved(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	cut := make(chan struct{})
	runner.answerWith(func(ctx context.Context, _ string) (Output, error) {
		<-ctx.Done()
		close(cut)
		return Output{}, ctx.Err()
	})
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	svc.sweep()
	waitUntil(t, "the start never reached the machine", func() bool { return len(runner.restarts()) == 1 })
	backend.set(func(b *restartBackend) { b.watched = nil })
	svc.sweep()
	select {
	case <-cut:
	case <-time.After(5 * time.Second):
		t.Fatal("the start for a removed machine was not cut off")
	}
	svc.autoRuns.Wait()
	if state, err := svc.RestartStatus(t.Context(), "node-1"); err != nil || state.AutoStart != nil {
		t.Fatalf("a removed machine is still watched: %#v %v", state, err)
	}
	clock.Advance(time.Hour)
	sweepOnce(svc)
	if scripts := runner.restarts(); len(scripts) != 1 {
		t.Fatalf("a removed machine was started %d more times", len(scripts)-1)
	}
	if records := backend.recorded(); len(records) != 0 {
		t.Fatalf("a cut-off start was recorded: %#v", records)
	}
}

// Failures are forgotten once the machine has answered for ten minutes,
// even after automatic start stopped for it; a machine that answers for
// less keeps its count.
func TestAutoStartForgetsFailuresOnceAMachineStaysOnline(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	runner.answerWith(peerExits(28))
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	for _, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute} {
		clock.Advance(wait)
		sweepOnce(svc)
	}
	if state := autoState(t, svc, "node-1"); state.State != "stopped" {
		t.Fatalf("automatic start did not stop: %#v", state)
	}
	backend.set(func(b *restartBackend) { b.answering = map[string]bool{"node-1": true} })
	sweepOnce(svc)
	clock.Advance(autoStartSettle - time.Second)
	sweepOnce(svc)
	if state := autoState(t, svc, "node-1"); state.State != "stopped" || state.Attempts != autoStartLimit {
		t.Fatalf("failures were forgotten before the machine stayed online %s: %#v", autoStartSettle, state)
	}
	clock.Advance(time.Second)
	sweepOnce(svc)
	if state := autoState(t, svc, "node-1"); state.State != "watching" || state.Attempts != 0 || state.LastError != "" || !state.OfflineSince.IsZero() {
		t.Fatalf("failures were not forgotten after the machine stayed online %s: %#v", autoStartSettle, state)
	}
	runner.answerWith(peerStarted)
	backend.set(func(b *restartBackend) { b.answering = nil })
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	if scripts := runner.restarts(); len(scripts) != autoStartLimit+1 {
		t.Fatalf("automatic start did not resume: %d runs", len(scripts))
	}
	if state := autoState(t, svc, "node-1"); state.State != "watching" || state.Attempts != 1 {
		t.Fatalf("after the resumed start: %#v", state)
	}
}

// A machine that answers only briefly between failures keeps its count.
func TestAutoStartKeepsCountingAcrossABriefReturn(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	runner.answerWith(peerExits(28))
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	backend.set(func(b *restartBackend) { b.answering = map[string]bool{"node-1": true} })
	sweepOnce(svc)
	clock.Advance(autoStartSettle / 2)
	sweepOnce(svc)
	backend.set(func(b *restartBackend) { b.answering = nil })
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	if state := autoState(t, svc, "node-1"); state.Attempts != 2 || state.State != "retrying" {
		t.Fatalf("a brief return reset the count: %#v", state)
	}
}

// A manual restart that brings the machine back resumes automatic start
// for it with a clean count.
func TestAManualRestartResumesAutomaticStart(t *testing.T) {
	svc, runner, _, clock := autoStartFixture(t)
	runner.answerWith(peerExits(28))
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	for _, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute} {
		clock.Advance(wait)
		sweepOnce(svc)
	}
	if state := autoState(t, svc, "node-1"); state.State != "stopped" {
		t.Fatalf("automatic start did not stop: %#v", state)
	}
	runner.answerWith(peerExits(28))
	if _, err := svc.Restart(t.Context(), "node-1"); err == nil {
		t.Fatal("the failing manual restart succeeded")
	}
	if state := autoState(t, svc, "node-1"); state.State != "stopped" || state.Attempts != autoStartLimit {
		t.Fatalf("a failed manual restart changed automatic start: %#v", state)
	}
	runner.answerWith(peerRestarted)
	if _, err := svc.Restart(t.Context(), "node-1"); err != nil {
		t.Fatal(err)
	}
	if state := autoState(t, svc, "node-1"); state.State != "watching" || state.Attempts != 0 || state.LastError != "" {
		t.Fatalf("a manual restart did not resume automatic start: %#v", state)
	}
}

// Automatic start leaves a machine alone while it is being upgraded or
// restarted by hand, and a manual restart or an upgrade is refused while
// automatic start runs for it.
func TestAutoStartDoesNotRunBesideAnUpgradeOrAManualRestart(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	zh := i18n.New(i18n.LocaleZH)
	ctx := i18n.WithLocale(t.Context(), i18n.LocaleZH)
	sweepOnce(svc)
	clock.Advance(autoStartAfter)

	backend.hold = make(chan struct{})
	upgraded := make(chan error, 1)
	go func() { _, err := svc.Upgrade(context.Background(), "node-1"); upgraded <- err }()
	waitUntil(t, "the upgrade never reached the backend", func() bool { return backend.holding.Load() == 1 })
	sweepOnce(svc)
	if scripts := runner.restarts(); len(scripts) != 0 {
		t.Fatal("automatic start ran beside an upgrade")
	}
	close(backend.hold)
	if err := <-upgraded; err != nil {
		t.Fatal(err)
	}

	backend.set(func(b *restartBackend) { b.restartHold = make(chan struct{}) })
	restarted := make(chan error, 1)
	go func() { _, err := svc.Restart(context.Background(), "node-1"); restarted <- err }()
	waitUntil(t, "the manual restart never reached the backend", func() bool { return backend.restartHolding.Load() == 1 })
	sweepOnce(svc)
	if scripts := runner.restarts(); len(scripts) != 1 {
		t.Fatalf("automatic start ran beside a manual restart: %d runs", len(scripts))
	}
	backend.set(func(b *restartBackend) { close(b.restartHold); b.restartHold = nil })
	if err := <-restarted; err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	runner.answerWith(func(ctx context.Context, script string) (Output, error) {
		<-release
		return peerStarted(ctx, script)
	})
	svc.sweep()
	waitUntil(t, "automatic start never reached the machine", func() bool { return len(runner.restarts()) == 2 })
	state, err := svc.RestartStatus(ctx, "node-1")
	if err != nil || state.Restart == nil || !state.Automatic || state.Restart.Status != "installing" || state.AutoStart == nil || state.AutoStart.State != "attempting" {
		t.Fatalf("status during automatic start = %#v %v", state, err)
	}
	refused := func(err error) bool {
		var step *StepError
		return errors.As(err, &step) && step.Code == "in_progress" && step.Message == zh.T(i18n.SSHRestartRunning)
	}
	if _, err := svc.Restart(ctx, "node-1"); !refused(err) {
		t.Fatalf("manual restart during automatic start: %v", err)
	}
	if _, err := svc.Upgrade(ctx, "node-1"); !refused(err) {
		t.Fatalf("upgrade during automatic start: %v", err)
	}
	close(release)
	svc.autoRuns.Wait()
	if scripts := runner.restarts(); len(scripts) != 2 || scripts[1].script != startIfStopped {
		t.Fatalf("expected one automatic start beside the manual restart, got %d runs", len(scripts))
	}
}

// Automatic start does not try a machine whose SSH session is down, and
// a machine it cannot reach over SSH is not counted as a failed start.
func TestAutoStartDoesNotTryAMachineItCannotReach(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	backend.set(func(b *restartBackend) { b.unreachable = map[string]bool{"node-1": true} })
	sweepOnce(svc)
	clock.Advance(time.Hour)
	sweepOnce(svc)
	if state := autoState(t, svc, "node-1"); state.State != "unreachable" || state.Attempts != 0 {
		t.Fatalf("a machine without an SSH session: %#v", state)
	}
	if len(runner.restarts()) != 0 || len(runner.calls) != 0 {
		t.Fatal("a machine without an SSH session was contacted")
	}
	backend.set(func(b *restartBackend) { b.unreachable = nil })
	runner.stderr = "ssh: connect to host dev.example port 22: Connection refused"
	sweepOnce(svc)
	if state := autoState(t, svc, "node-1"); state.State != "unreachable" || state.Attempts != 0 || state.LastError == "" {
		t.Fatalf("a machine SSH could not reach: %#v", state)
	}
	if len(runner.restarts()) != 0 || len(backend.recorded()) != 0 {
		t.Fatal("a machine SSH could not reach was started or recorded")
	}
	runner.stderr = ""
	clock.Advance(autoStartBackoff)
	sweepOnce(svc)
	if scripts := runner.restarts(); len(scripts) != 1 {
		t.Fatalf("a machine reachable again was not started: %d runs", len(scripts))
	}
}

// A machine that answers the cluster is only watched.
func TestAutoStartLeavesAnAnsweringMachineAlone(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	backend.set(func(b *restartBackend) { b.answering = map[string]bool{"node-1": true} })
	for range 10 {
		sweepOnce(svc)
		clock.Advance(10 * time.Minute)
	}
	if state := autoState(t, svc, "node-1"); state.State != "watching" || !state.OfflineSince.IsZero() {
		t.Fatalf("an answering machine: %#v", state)
	}
	if len(runner.restarts()) != 0 || len(runner.calls) != 0 {
		t.Fatal("an answering machine was contacted")
	}
}

// Automatic start looks at the watched machines as soon as it starts,
// and closing the service ends it together with a start in flight.
func TestClosingTheServiceEndsAutomaticStart(t *testing.T) {
	svc, runner, _, clock := autoStartFixture(t)
	svc.AutoStart()
	waitUntil(t, "automatic start never looked at the machine", func() bool {
		state, err := svc.RestartStatus(t.Context(), "node-1")
		return err == nil && state.AutoStart != nil && state.AutoStart.State == "waiting"
	})
	cut := make(chan struct{})
	runner.answerWith(func(ctx context.Context, _ string) (Output, error) {
		<-ctx.Done()
		close(cut)
		return Output{}, ctx.Err()
	})
	clock.Advance(autoStartAfter)
	svc.sweep()
	waitUntil(t, "the start never reached the machine", func() bool { return len(runner.restarts()) == 1 })
	closed := make(chan error, 1)
	go func() { closed <- svc.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the service waited on the start in flight")
	}
	select {
	case <-cut:
	default:
		t.Fatal("the start in flight was not cut off")
	}
	svc.sweep()
	svc.autoRuns.Wait()
	if scripts := runner.restarts(); len(scripts) != 1 {
		t.Fatalf("a closed service started %d more machines", len(scripts)-1)
	}
}

// The log of an automatic start says it was automatic and that a running
// peer is not ended.
func TestAutoStartSaysWhatItDoes(t *testing.T) {
	svc, _, _, clock := autoStartFixture(t)
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	state, err := svc.RestartStatus(t.Context(), "node-1")
	if err != nil || state.Restart == nil {
		t.Fatalf("%#v %v", state, err)
	}
	if !strings.Contains(logText(*state.Restart), i18n.New(i18n.LocaleZH).T(i18n.SSHAutoStarting)) {
		t.Fatalf("the automatic start does not say what it does:\n%s", logText(*state.Restart))
	}
}
