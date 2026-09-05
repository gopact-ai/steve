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
