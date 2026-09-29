package sshconnect

import (
	"context"
	"encoding/json"
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

// cutOffAnswer is a restart script that runs until its context ends, and
// closes cut then. One still running when the test ends gives up, so a
// start that is never cut off fails the test at once rather than holding
// the service's Close for as long as the script may run.
func cutOffAnswer(t *testing.T) (answer func(context.Context, string) (Output, error), cut <-chan struct{}) {
	ended, closed := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(ended) })
	var once sync.Once
	return func(ctx context.Context, _ string) (Output, error) {
		select {
		case <-ctx.Done():
			once.Do(func() { close(closed) })
			return Output{}, ctx.Err()
		case <-ended:
			return Output{}, errors.New("the test ended before the start was cut off")
		}
	}, closed
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

// startUntilGivenUp starts a machine's peer as many times as automatic
// start makes starts, each coming back and dying again two minutes later,
// and looks at the machine once more after the last one died.
func startUntilGivenUp(t *testing.T, svc *Service, runner *restartRunner, backend *restartBackend, clock *fakeClock) {
	t.Helper()
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	for start := 1; start <= autoStartLimit; start++ {
		if scripts := runner.restarts(); len(scripts) != start {
			t.Fatalf("expected %d starts, got %d", start, len(scripts))
		}
		backend.set(func(b *restartBackend) { b.answering = map[string]bool{"node-1": true} })
		sweepOnce(svc)
		clock.Advance(2 * time.Minute)
		backend.set(func(b *restartBackend) { b.answering = nil })
		sweepOnce(svc)
		clock.Advance(8 * time.Minute)
		sweepOnce(svc)
	}
}

// A peer that starts and comes back but dies again before the machine has
// stayed online for ten minutes counts against the limit like a failed
// start: after five such starts automatic start stops for the machine,
// says why and records that it stopped, once.
func TestAutoStartGivesUpOnAPeerThatKeepsDying(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	zh := i18n.New(i18n.LocaleZH)
	startUntilGivenUp(t, svc, runner, backend, clock)
	if scripts := runner.restarts(); len(scripts) != autoStartLimit {
		t.Fatalf("automatic start went past its limit: %d starts", len(scripts))
	}
	state := autoState(t, svc, "node-1")
	gaveUp := zh.T(i18n.SSHAutoStartGaveUp, autoStartLimit, int(autoStartSettle/time.Minute))
	if state.State != "stopped" || state.Attempts != autoStartLimit || state.LastError != gaveUp {
		t.Fatalf("after a peer that kept dying: %#v", state)
	}
	records := backend.recorded()
	if len(records) != autoStartLimit+1 {
		t.Fatalf("records = %#v, want each start and the stop", records)
	}
	for _, record := range records[:autoStartLimit] {
		if record.Outcome != RestartStarted {
			t.Fatalf("records = %#v", records)
		}
	}
	if stop := records[autoStartLimit]; !stop.Automatic || stop.NodeID != "node-1" || stop.Outcome != RestartStopped || stop.Reason != gaveUp || !stop.At.Equal(clock.Now().UTC()) {
		t.Fatalf("the stop was recorded as %#v", stop)
	}
	clock.Advance(time.Hour)
	sweepOnce(svc)
	if records := backend.recorded(); len(records) != autoStartLimit+1 {
		t.Fatalf("the stop was recorded again: %#v", records[autoStartLimit:])
	}
}

// A stop that could not be recorded when automatic start gave up is
// recorded on a later look, once.
func TestAutoStartRecordsAStopItCouldNotRecordAtFirstLater(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	backend.set(func(b *restartBackend) { b.recordErr = errors.New("集群暂时没有 leader") })
	startUntilGivenUp(t, svc, runner, backend, clock)
	if state := autoState(t, svc, "node-1"); state.State != "stopped" {
		t.Fatalf("automatic start did not stop: %#v", state)
	}
	backend.set(func(b *restartBackend) { b.recordErr = nil })
	clock.Advance(autoStartEvery)
	sweepOnce(svc)
	sweepOnce(svc)
	records := backend.recorded()
	if len(records) != 1 || records[0].Outcome != RestartStopped || !records[0].Automatic {
		t.Fatalf("records = %#v, want the stop once", records)
	}
}

// A machine that leaves the cluster is dropped at once: the start running
// for it is cut off and nothing is recorded or tried for it again.
func TestAutoStartStopsForAMachineThatWasRemoved(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	answer, cut := cutOffAnswer(t)
	runner.answerWith(answer)
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

// heldLook holds the next look automatic start takes at a machine until
// released, so the look can be taken before something happens to the
// machine and read after it.
type heldLook struct {
	*restartBackend
	mu     sync.Mutex
	gate   chan struct{}
	looked chan struct{}
}

func (b *heldLook) hold() (looked <-chan struct{}, release func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gate, b.looked = make(chan struct{}), make(chan struct{})
	gate := b.gate
	return b.looked, func() { close(gate) }
}

func (b *heldLook) Answers(ctx context.Context, nodeID string) bool {
	b.mu.Lock()
	gate, looked := b.gate, b.looked
	b.gate, b.looked = nil, nil
	b.mu.Unlock()
	answers := b.restartBackend.Answers(ctx, nodeID)
	if gate != nil {
		close(looked)
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	return answers
}

// A look automatic start took at a machine before a manual restart or an
// upgrade of it settled saw the machine as it was before: it is set aside
// rather than taken for the machine being offline all along, and the
// machine's time offline counts afresh from the next look. No start is
// made beside the peer that was just started.
func TestAutoStartSetsAsideALookTakenBeforeAManualRestartOrAnUpgradeSettled(t *testing.T) {
	for _, operation := range []struct {
		name    string
		run     func(*Service) error
		scripts int
	}{
		{name: "manual restart", run: func(svc *Service) error { _, err := svc.Restart(context.Background(), "node-1"); return err }, scripts: 1},
		{name: "upgrade", run: func(svc *Service) error { _, err := svc.Upgrade(context.Background(), "node-1"); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			svc, runner, inner, clock := autoStartFixture(t)
			backend := &heldLook{restartBackend: inner}
			svc.backend = backend
			sweepOnce(svc)
			clock.Advance(autoStartAfter)
			looked, release := backend.hold()
			swept := make(chan struct{})
			go func() { defer close(swept); sweepOnce(svc) }()
			<-looked
			clock.Advance(time.Second)
			runner.answerWith(peerRestarted)
			if err := operation.run(svc); err != nil {
				t.Fatal(err)
			}
			inner.set(func(b *restartBackend) { b.answering = map[string]bool{"node-1": true} })
			runner.answerWith(peerStarted)
			release()
			<-swept
			if scripts := runner.restarts(); len(scripts) != operation.scripts {
				t.Fatalf("a look from before the %s started the machine: %d restart scripts, want %d", operation.name, len(scripts), operation.scripts)
			}
			for _, record := range inner.recorded() {
				if record.Automatic {
					t.Fatalf("an automatic start was recorded: %#v", inner.recorded())
				}
			}
			if state := autoState(t, svc, "node-1"); state.State != "watching" || !state.OfflineSince.IsZero() {
				t.Fatalf("after the %s: %#v", operation.name, state)
			}
			inner.set(func(b *restartBackend) { b.answering = nil })
			clock.Advance(time.Second)
			sweepOnce(svc)
			since := clock.Now()
			clock.Advance(autoStartAfter - time.Second)
			sweepOnce(svc)
			if state := autoState(t, svc, "node-1"); state.State != "waiting" || !state.OfflineSince.Equal(since) {
				t.Fatalf("time offline did not count afresh after the %s: %#v", operation.name, state)
			}
			if scripts := runner.restarts(); len(scripts) != operation.scripts {
				t.Fatalf("a machine offline for less than %s since the %s was started", autoStartAfter, operation.name)
			}
		})
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
	// The manual restart just started the peer: the machine's time offline
	// counts afresh.
	sweepOnce(svc)
	clock.Advance(autoStartAfter)

	release := make(chan struct{})
	runner.answerWith(func(ctx context.Context, script string) (Output, error) {
		select {
		case <-release:
			return peerStarted(ctx, script)
		case <-ctx.Done():
			return Output{}, ctx.Err()
		}
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
	// A restart let through would run its script into the held answer;
	// the deadline makes that a failure rather than a hang.
	asked, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := svc.Restart(asked, "node-1"); !refused(err) {
		t.Fatalf("manual restart during automatic start: %v", err)
	}
	if _, err := svc.Upgrade(asked, "node-1"); !refused(err) {
		t.Fatalf("upgrade during automatic start: %v", err)
	}
	close(release)
	svc.autoRuns.Wait()
	if scripts := runner.restarts(); len(scripts) != 2 || scripts[1].script != startIfStopped {
		t.Fatalf("expected one automatic start beside the manual restart, got %d runs", len(scripts))
	}
}

// A machine whose installation lock another installation holds, or one
// left behind, blocks automatic start: nothing counts against the limit,
// and it keeps trying, backing off from a minute to every ten, so the
// machine starts once the lock is gone. The machine shows why and how to
// clear a lock left behind, and each time it becomes blocked is recorded
// once, not once a try. The start that follows once the lock is free is
// recorded and counted as usual.
func TestAutoStartKeepsTryingAMachineTheInstallationLockBlocks(t *testing.T) {
	svc, runner, backend, clock := autoStartFixture(t)
	runner.answerWith(peerExits(21))
	zh := i18n.New(i18n.LocaleZH)
	for _, text := range []i18n.Catalog{zh, i18n.New(i18n.LocaleEN)} {
		if fix := text.T(i18n.SSHRestartBusyFix); !strings.Contains(fix, "~/steve-bin/.install-lock") {
			t.Fatalf("the fix for a held installation lock does not say which lock a left-behind one is: %q", fix)
		}
	}
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	sweepOnce(svc)
	waits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for try, wait := range waits {
		state := autoState(t, svc, "node-1")
		if state.State != "blocked" || state.Attempts != 0 || state.LastError != zh.T(i18n.SSHRestartBusy) || !state.NextAt.Equal(clock.Now().Add(wait)) {
			t.Fatalf("after try %d: %#v", try+1, state)
		}
		if fix := autoSuggestion(t, svc, "node-1"); fix != zh.T(i18n.SSHRestartBusyFix) {
			t.Fatalf("after try %d the machine suggests %q", try+1, fix)
		}
		clock.Advance(wait - time.Second)
		sweepOnce(svc)
		if scripts := runner.restarts(); len(scripts) != try+1 {
			t.Fatalf("try %d was followed within %s", try+1, wait)
		}
		clock.Advance(time.Second)
		sweepOnce(svc)
		if scripts := runner.restarts(); len(scripts) != try+2 {
			t.Fatalf("try %d was not followed after %s: %d runs", try+1, wait, len(scripts))
		}
	}
	if records := backend.recorded(); len(records) != 1 || records[0].Outcome != RestartFailed || !records[0].Automatic || records[0].Reason != zh.T(i18n.SSHRestartBusy) {
		t.Fatalf("records while the lock is held = %#v", records)
	}
	runner.answerWith(peerStarted)
	clock.Advance(waits[len(waits)-1])
	sweepOnce(svc)
	if state := autoState(t, svc, "node-1"); state.State != "watching" || state.Attempts != 1 || state.LastError != "" || autoSuggestion(t, svc, "node-1") != "" {
		t.Fatalf("after the lock was freed: %#v", state)
	}
	if records := backend.recorded(); len(records) != 2 || records[1].Outcome != RestartStarted || !records[1].Automatic {
		t.Fatalf("records once the lock was freed = %#v", records)
	}
	runner.answerWith(peerExits(21))
	clock.Advance(autoStartBackoff)
	sweepOnce(svc)
	if state := autoState(t, svc, "node-1"); state.State != "blocked" || state.Attempts != 1 || !state.NextAt.Equal(clock.Now().Add(autoStartBackoff)) {
		t.Fatalf("blocked again: %#v", state)
	}
	if records := backend.recorded(); len(records) != 3 || records[2].Outcome != RestartFailed || records[2].Reason != zh.T(i18n.SSHRestartBusy) {
		t.Fatalf("becoming blocked again was not recorded: %#v", records)
	}
}

// autoSuggestion is what the fleet is told to do about how automatic
// start stands for nodeID.
func autoSuggestion(t *testing.T, svc *Service, nodeID string) string {
	t.Helper()
	raw, err := json.Marshal(autoState(t, svc, nodeID))
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Suggestion string `json:"suggestion"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields.Suggestion
}

// Automatic start does not try a machine whose SSH session is down. A
// machine it cannot open an SSH session to blocks it, as a held
// installation lock does: that counts as no failed start, is recorded
// once, and is tried again.
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
	state := autoState(t, svc, "node-1")
	if state.State != "blocked" || state.Attempts != 0 || state.LastError == "" || !state.NextAt.Equal(clock.Now().Add(autoStartBackoff)) || autoSuggestion(t, svc, "node-1") == "" {
		t.Fatalf("a machine SSH could not reach: %#v", state)
	}
	if len(runner.restarts()) != 0 {
		t.Fatal("a machine SSH could not reach was started")
	}
	if records := backend.recorded(); len(records) != 1 || records[0].Outcome != RestartFailed || !records[0].Automatic || records[0].Reason != state.LastError {
		t.Fatalf("records of a machine SSH could not reach = %#v", records)
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
	answer, cut := cutOffAnswer(t)
	runner.answerWith(answer)
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

// Closing the service also ends an automatic start waiting for the
// machine to come back: it stopped nothing, so nothing is left half done.
func TestClosingTheServiceEndsAnAutomaticStartWaitingForTheMachine(t *testing.T) {
	svc, _, backend, clock := autoStartFixture(t)
	backend.set(func(b *restartBackend) { b.restartHold = make(chan struct{}) })
	sweepOnce(svc)
	clock.Advance(autoStartAfter)
	svc.sweep()
	waitUntil(t, "the start never waited for the machine", func() bool { return backend.restartHolding.Load() == 1 })
	closed := make(chan error, 1)
	go func() { closed <- svc.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		backend.set(func(b *restartBackend) { close(b.restartHold); b.restartHold = nil })
		t.Fatal("closing the service waited on an automatic start waiting for the machine")
	}
	if nodes := backend.restartedNodes(); len(nodes) != 0 {
		t.Fatalf("a closed service still saw %v come back", nodes)
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
