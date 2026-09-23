package artifact

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// sendTick hands a renewal tick to whoever is renewing, or fails the test
// when nobody is.
func sendTick(t *testing.T, ticks chan time.Time) {
	t.Helper()
	select {
	case ticks <- time.Time{}:
	case <-time.After(5 * time.Second):
		t.Fatal("nobody is renewing the canonical lock")
	}
}

// A landing with no driver of its own — a direct Land, or a recovery
// run by the periodic retry — still holds the canonical lock for as long
// as it works, not for one TTL from when it took it.
func TestCanonicalLockOutlivesItsTTLWithoutADriver(t *testing.T) {
	clock := &leaseClock{t: time.Now()}
	store, p := newStoreWith(t, &localNode{}, project.Home{Path: t.TempDir()}, ledger.Options{Now: clock.now})
	ticks := make(chan time.Time)
	store.renewTicks = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	land := Landing{ID: "land-test", Project: p.ID}
	unlock, err := store.lockCanonical(t.Context(), p, &land, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(landTTL / 2)
	sendTick(t, ticks)
	sendTick(t, ticks)
	clock.advance(landTTL/2 + landTTL/6)
	if _, err := store.ledger.Acquire(t.Context(), "canonical:"+p.ID, "other", landTTL); !errors.Is(err, ledger.ErrHeld) {
		t.Fatalf("canonical lock expired under a running landing: %v", err)
	}
	unlock()
	if _, err := store.ledger.Acquire(t.Context(), "canonical:"+p.ID, "other", landTTL); err != nil {
		t.Fatalf("canonical lock kept after the landing: %v", err)
	}
}

// A recovery takes the canonical lock under a name of its own. The record
// has to say so before anything is written: a process that dies mid-
// recovery leaves that lock behind, and boot recovery only releases the
// lock the landing recorded — otherwise the project waits out the TTL.
func TestRecoveryRecordsItsLockBeforeWriting(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := crashMidApply(t, store, p, canonical)
	if err := store.pendRecovery(ctx, &land); err != nil {
		t.Fatal(err)
	}
	// The snapshot before recovery fails, the way a crash at that moment
	// would stop it: after the lock was taken, before any path is written.
	moved := canonical + ".away"
	if err := os.Rename(canonical, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := store.recoverLanding(ctx, land); err == nil {
		t.Fatal("recovery without its directory succeeded")
	}
	if err := os.Rename(moved, canonical); err != nil {
		t.Fatal(err)
	}
	op, _, err := store.ledger.Operation(ctx, land.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored Landing
	if err := json.Unmarshal(op.Data, &stored); err != nil {
		t.Fatal(err)
	}
	if op.State != LandRecoveryPending || stored.Lease == nil || !strings.HasPrefix(stored.Lease.Holder, "recovery:"+land.ID+":") || stored.Borrowed {
		t.Fatalf("state %s, recorded lease %+v borrowed=%v", op.State, stored.Lease, stored.Borrowed)
	}
}

// orphanApplying leaves a landing in applying the way a failed apply whose
// move to recovery-pending was refused does: its lock is gone, and nobody
// is applying it any more.
func orphanApplying(t *testing.T, store *Store, p project.Project, canonical string, live bool) (Landing, string) {
	t.Helper()
	lease, err := store.ledger.Acquire(t.Context(), "canonical:"+p.ID, "lost-landing", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		if err := store.ledger.Release(t.Context(), lease); err != nil {
			t.Fatal(err)
		}
	}
	return crashMidApplyAs(t, store, p, canonical, func(l *Landing) { l.Lease = &lease })
}

func TestAnOrphanedApplyingLandingBlocksNewLandingsAndIsRetried(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, merged := orphanApplying(t, store, p, canonical, false)

	if _, err := store.Land(ctx, p, land.Artifact, "again"); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("land over an orphaned apply = %v, want ErrRecoveryPending", err)
	}
	if read(t, canonical, "b") != "b0" {
		t.Fatal("a new landing wrote over an orphaned one")
	}
	retried, err := store.RetryRecoveries(ctx)
	if err != nil || len(retried) != 1 || retried[0].ID != land.ID || retried[0].State != LandCommitted {
		t.Fatalf("retry = %+v err=%v", retried, err)
	}
	if read(t, canonical, "b") != "b1" || read(t, canonical, "c") != "c1" {
		t.Fatalf("after retry b=%q c=%q", read(t, canonical, "b"), read(t, canonical, "c"))
	}
	if ref, _, _ := store.Resolve(ctx, CanonicalRef("p")); ref.Artifact != merged {
		t.Fatalf("canonical = %s, want %s", ref.Artifact, merged)
	}
}

// A landing still applying under a live lock is someone's work in
// progress, not a recovery to take over.
func TestTheRetryLeavesALiveApplyingLandingAlone(t *testing.T) {
	ctx := t.Context()
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := orphanApplying(t, store, p, canonical, true)
	if retried, err := store.RetryRecoveries(ctx); err != nil || len(retried) != 0 {
		t.Fatalf("retry = %+v err=%v", retried, err)
	}
	if state := landingState(t, store, land.ID); state != LandApplying {
		t.Fatalf("state = %s, want %s", state, LandApplying)
	}
}

// A queued result whose landing went recovery-pending is landed by the
// recovery. It must leave the queue then, not land a second time as an
// empty landing on the next pass.
func TestRecoveredQueuedResultLeavesTheQueue(t *testing.T) {
	ctx := t.Context()
	store, p, nodes, canonical, artifact := nodeLanding(t)
	if err := store.Defer(ctx, "p", artifact, "test"); err != nil {
		t.Fatal(err)
	}
	nodes.before = func(req ops.Request) {
		if req.Op == ops.Apply {
			write(t, canonical, "a", "a1")
			nodes.down, nodes.before = true, nil
		}
	}
	if _, err := store.LandPending(ctx, p); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("land pending = %v, want ErrRecoveryPending", err)
	}
	nodes.down = false
	if recovered, err := store.RetryRecoveries(ctx); err != nil || len(recovered) != 1 || recovered[0].State != LandCommitted {
		t.Fatalf("retry = %+v err=%v", recovered, err)
	}
	again, err := store.LandPending(ctx, p)
	if err != nil || len(again) != 0 {
		t.Fatalf("the recovered result landed again: %+v err=%v", again, err)
	}
}
