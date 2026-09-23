package artifact

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
