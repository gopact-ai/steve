package idle

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIdleTimeoutRunsOutOnSilenceNotOnWork(t *testing.T) {
	ctx, stop, touch := WithTimeout(context.Background(), 60*time.Millisecond)
	defer stop()
	// Busy for longer than the timeout, never silent for it.
	for i := 0; i < 6; i++ {
		time.Sleep(25 * time.Millisecond)
		touch()
		if ctx.Err() != nil {
			t.Fatalf("a working turn was cut at step %d: %v", i, ctx.Err())
		}
	}
	select {
	case <-ctx.Done():
	case <-time.After(300 * time.Millisecond):
		t.Fatal("a silent turn never timed out")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", ctx.Err())
	}
}

func TestIdleTimeoutFollowsItsParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop, _ := WithTimeout(parent, time.Hour)
	defer stop()
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not reach the idle context")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("err = %v", ctx.Err())
	}
}

func TestPausePreservesRemainingAndTouchWhilePaused(t *testing.T) {
	ctx, stop, touch := WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	time.Sleep(80 * time.Millisecond)
	ctx.Pause()
	clock := ctx.(*idleContext)
	clock.mu.Lock()
	remaining := clock.remaining
	clock.mu.Unlock()
	ctx.Pause()
	time.Sleep(230 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("paused clock expired")
	}
	start := time.Now()
	ctx.Resume()
	ctx.Resume()
	<-ctx.Done()
	if elapsed := time.Since(start); elapsed < remaining-20*time.Millisecond || elapsed > remaining+100*time.Millisecond {
		t.Fatalf("resumed for %s; remaining %s", elapsed, remaining)
	}
	// A buffered progress report resets silence while paused, without
	// accidentally starting the clock again.
	ctx2, stop2, touch2 := WithTimeout(context.Background(), 40*time.Millisecond)
	defer stop2()
	ctx2.Pause()
	touch2()
	time.Sleep(60 * time.Millisecond)
	if ctx2.Err() != nil {
		t.Fatal(ctx2.Err())
	}
	ctx2.Resume()
	select {
	case <-ctx2.Done():
		t.Fatal("touch lost while paused")
	case <-time.After(15 * time.Millisecond):
	}
	touch() // An expired context cannot be revived.
	if ctx.Err() == nil {
		t.Fatal("revived expired clock")
	}
}

func TestPauseDoesNotSuspendHardDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	ctx, stop, _ := WithTimeout(parent, time.Hour)
	defer stop()
	ctx.Pause()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("hard deadline was paused")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal(ctx.Err())
	}
}

// A question waiting on a person is not silence. The hold is found
// through any context derived from the clock, and a connection coming
// back does not restart a clock that a pending question still holds.
func TestHoldSuspendsSilenceAcrossPauseAndResume(t *testing.T) {
	ctx, stop, _ := WithTimeout(context.Background(), 40*time.Millisecond)
	defer stop()
	derived, cancel := context.WithCancel(ctx)
	defer cancel()
	release := Hold(derived)
	ctx.Pause()
	ctx.Resume()
	time.Sleep(90 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("held clock expired: %v", ctx.Err())
	}
	released := time.Now()
	release()
	// The answer is a sign of life: the whole silence starts again.
	select {
	case <-ctx.Done():
		if elapsed := time.Since(released); elapsed < 40*time.Millisecond {
			t.Fatalf("released clock expired after %v, before a full silence", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("released clock never expired")
	}
}

func TestNestedHoldsAndPauseKeepClockStopped(t *testing.T) {
	ctx, stop, _ := WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	first, second := Hold(ctx), Hold(ctx)
	first()
	first() // Releasing twice must not drop the other holder's hold.
	time.Sleep(60 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("second hold was dropped by the first release")
	}
	ctx.Pause()
	second()
	time.Sleep(60 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("releasing a hold restarted a paused clock")
	}
	ctx.Resume()
	select {
	case <-ctx.Done():
	case <-time.After(300 * time.Millisecond):
		t.Fatal("resumed clock never expired")
	}
}

func TestHoldWithoutClockIsHarmless(t *testing.T) {
	Hold(context.Background())()
}

// Expired tells the silence clock running out from every other way a
// context under it can end, and a context derived before the end keeps the
// reason it ended for first.
func TestExpiredIsOnlyTheSilenceRunningOut(t *testing.T) {
	silent, stop, _ := WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	derived, cancel := context.WithCancel(silent)
	defer cancel()
	<-derived.Done()
	if !Expired(silent) || !Expired(derived) || !errors.Is(derived.Err(), context.DeadlineExceeded) {
		t.Fatalf("silence: expired=%v/%v err=%v", Expired(silent), Expired(derived), derived.Err())
	}
	lost, lose := context.WithCancel(context.Background())
	// Lost before the clock starts, so its timer cannot run out first.
	lose()
	clock, stopClock, _ := WithTimeout(lost, 10*time.Millisecond)
	defer stopClock()
	early, cancelEarly := context.WithCancel(clock)
	cancelEarly()
	<-clock.Done()
	time.Sleep(20 * time.Millisecond)
	if Expired(clock) || Expired(early) {
		t.Fatal("a lost parent or an earlier cancel read as silence")
	}
	deadline, cancelDeadline := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelDeadline()
	bounded, stopBounded, _ := WithTimeout(deadline, time.Hour)
	defer stopBounded()
	<-bounded.Done()
	stopped, stopNow, _ := WithTimeout(context.Background(), time.Hour)
	stopNow()
	if Expired(bounded) || Expired(stopped) || Expired(context.Background()) {
		t.Fatal("a parent deadline, a stop or no clock read as silence")
	}
}

// A context derived from a clock before it ended has ended, for the
// clock's reason, by the time anyone can see that the clock has: what is
// read on it after the clock ran out or was stopped is not read on a
// context still ending. The contexts are derived while the clock is held,
// so they are derived before its own timer can run it out.
func TestDerivedContextsEndWithTheClock(t *testing.T) {
	for run := 0; run < 100; run++ {
		silent, stop, _ := WithTimeout(context.Background(), time.Millisecond)
		release := Hold(silent)
		derived, cancel := context.WithCancel(silent)
		bounded, cancelBounded := context.WithTimeout(silent, time.Hour)
		release()
		<-silent.Done()
		if !Expired(derived) || !Expired(bounded) || !errors.Is(derived.Err(), context.DeadlineExceeded) {
			t.Fatalf("run %d: the clock ran out; derived err=%v cause=%v, bounded cause=%v", run, derived.Err(), context.Cause(derived), context.Cause(bounded))
		}
		stop()
		cancel()
		cancelBounded()

		stopped, stopNow, _ := WithTimeout(context.Background(), time.Hour)
		derived, cancel = context.WithCancel(stopped)
		stopNow()
		if !errors.Is(context.Cause(derived), context.Canceled) {
			t.Fatalf("run %d: the clock was stopped; derived cause=%v", run, context.Cause(derived))
		}
		cancel()
	}
}

// A clock whose parent clock was stopped ends for that reason, even when its
// own silence runs out before it has heard: the parent's end is not silence.
func TestNestedClockEndsForItsParentsReason(t *testing.T) {
	for run := 0; run < 100; run++ {
		outer, stopOuter, _ := WithTimeout(context.Background(), time.Hour)
		inner, stopInner, _ := WithTimeout(outer, time.Hour)
		stopOuter()
		runOut(inner)
		if Expired(inner) || !errors.Is(inner.Err(), context.Canceled) {
			t.Fatalf("run %d: parent stopped; inner err=%v cause=%v", run, inner.Err(), context.Cause(inner))
		}
		stopInner()
	}
}

// runOut does what the clock's timer does when its silence runs out.
func runOut(ctx Context) {
	c := ctx.(*idleContext)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishLocked(context.DeadlineExceeded, errSilent)
}
