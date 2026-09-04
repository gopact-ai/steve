package turn

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIdleTimeoutRunsOutOnSilenceNotOnWork(t *testing.T) {
	ctx, stop, touch := withIdleTimeout(context.Background(), 60*time.Millisecond)
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
	ctx, stop, _ := withIdleTimeout(parent, time.Hour)
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
