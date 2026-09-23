package artifact

import (
	"errors"
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
