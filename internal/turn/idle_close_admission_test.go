package turn

import (
	"context"
	"errors"
	"testing"
)

func TestIdleClosePortKeepsOriginalSharedAdmissionOwner(t *testing.T) {
	original := &Coordinator{coordinatorState: &coordinatorState{}}
	view := &Coordinator{coordinatorState: original.coordinatorState}
	reserve := IdleCloseReservation(view)
	view.coordinatorState = &coordinatorState{}
	release, err := reserve(t.Context(), "conversation")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !original.busyWith("conversation") || view.busyWith("conversation") {
		t.Fatal("bound port copied or replaced its original conversation owner")
	}
	if _, err := IdleCloseReservation(original)(t.Context(), "conversation"); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("original shared fence allowed another closure: %v", err)
	}
}

func TestIdleClosePortRechecksMaintenanceAndReleasesFailureLocks(t *testing.T) {
	c := &Coordinator{coordinatorState: &coordinatorState{}}
	reserve := IdleCloseReservation(c)
	c.maintaining = true
	if _, err := reserve(t.Context(), "conversation"); err == nil {
		t.Fatal("port cached an earlier maintenance decision")
	}
	if !c.requestMu.TryLock() {
		t.Fatal("refused maintenance retained the request lock")
	}
	c.maintaining = false
	c.requestMu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reserve(ctx, "conversation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled original reservation returned %v", err)
	}
	if c.busyWith("conversation") || !c.requestMu.TryLock() {
		t.Fatal("cancelled reservation retained a fence or request lock")
	}
	c.requestMu.Unlock()
}

func TestIdleClosePortHoldsMaintenanceBoundaryUntilFenceRelease(t *testing.T) {
	c := &Coordinator{coordinatorState: &coordinatorState{}}
	reserve := IdleCloseReservation(c)
	release, err := reserve(t.Context(), "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if c.requestMu.TryLock() {
		c.requestMu.Unlock()
		release()
		t.Fatal("maintenance crossed an active automatic closure")
	}
	release()
	if c.busyWith("conversation") || !c.requestMu.TryLock() {
		t.Fatal("release left the original fence or maintenance boundary held")
	}
	c.requestMu.Unlock()
}
