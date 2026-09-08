package app

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestApplicationBackgroundJoinsOldGenerationAndRejectsLateCallbacks(t *testing.T) {
	background := newApplicationBackground(t.Context())
	started := make(chan struct{})
	var finished atomic.Bool
	background.Go(func(ctx context.Context) { close(started); <-ctx.Done(); finished.Store(true) })
	<-started
	background.Close()
	if !finished.Load() {
		t.Fatal("generation closed while its background worker was still running")
	}
	background.Go(func(context.Context) { t.Error("old callback started work after generation close") })
	background.Close()
}
