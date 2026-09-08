package artifact

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestLandOnceReturnsCommittedIdentityWithoutReapplying(t *testing.T) {
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	write(t, canonical, "file", "before")
	ws, err := store.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: "first"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws.Path, "file", "from-agent")
	result, _, err := store.Publish(t.Context(), ws, ws.Base, "first", "result")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.LandOnce(t.Context(), "stable", p, result.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	write(t, canonical, "file", "user-edit-after-completion")
	again, err := store.LandOnce(t.Context(), "stable", p, result.ID, "test")
	if err != nil || again.ID != first.ID || again.State != LandCommitted {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	if read(t, canonical, "file") != "user-edit-after-completion" {
		t.Fatal("replayed committed landing overwrote later edits")
	}
	all, err := store.Landings(t.Context(), p.ID)
	if err != nil || len(all) != 1 {
		t.Fatalf("duplicate landing: %+v err=%v", all, err)
	}
}

func TestLandOnceRetriesBusyAndInterruptedBeforeApply(t *testing.T) {
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	write(t, canonical, "file", "before")
	ws, _ := store.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: "first"})
	write(t, ws.Path, "file", "after")
	result, _, err := store.Publish(t.Context(), ws, ws.Base, "first", "result")
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.ledger.Acquire(t.Context(), "canonical:"+p.ID, "other", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.LandOnce(t.Context(), "stable", p, result.ID, "test")
	if !errors.Is(err, ledger.ErrHeld) || first.State != LandProposed {
		t.Fatalf("busy became terminal conflict: %+v err=%v", first, err)
	}
	if err := store.ledger.Release(t.Context(), held); err != nil {
		t.Fatal(err)
	}
	first.State = LandLocked
	if _, err := store.ledger.Transition(t.Context(), first.ID, LandProposed, LandLocked, "test", nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error { return tx.SetData(op, first) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverLandings(t.Context()); err != nil {
		t.Fatal(err)
	}
	land, err := store.LandOnce(t.Context(), "stable", p, result.ID, "test")
	if err != nil || land.State != LandCommitted || read(t, canonical, "file") != "after" {
		t.Fatalf("recovery=%+v err=%v", land, err)
	}
}

func TestLandOnceResumesApplying(t *testing.T) {
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := crashMidApply(t, store, p, canonical)
	land.Recoverable = true
	if _, err := store.ledger.Transition(t.Context(), land.ID, LandApplying, LandApplying, "test", nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error { return tx.SetData(op, land) }); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.LandOnce(t.Context(), land.ID, p, land.Artifact, "test")
	if err != nil || recovered.State != LandCommitted {
		t.Fatalf("recovery=%+v err=%v", recovered, err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if read(t, canonical, name) != name+"1" {
			t.Fatalf("%s not recovered", name)
		}
	}
}

func TestLandOnceDoesNotRetryARealMergeConflict(t *testing.T) {
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	write(t, canonical, "file", "base")
	ws, err := store.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: "work"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws.Path, "file", "agent change")
	result, _, err := store.Publish(t.Context(), ws, ws.Base, "work", "result")
	if err != nil {
		t.Fatal(err)
	}
	write(t, canonical, "file", "user change")
	for range 2 {
		land, err := store.LandOnce(t.Context(), "conflict", p, result.ID, "test")
		var conflict Conflict
		if !errors.As(err, &conflict) || land.State != LandMergeConflicted {
			t.Fatalf("real conflict was retried: land=%+v err=%v", land, err)
		}
		if read(t, canonical, "file") != "user change" {
			t.Fatal("conflicted landing overwrote user change")
		}
	}
	landings, err := store.Landings(t.Context(), p.ID)
	if err != nil || len(landings) != 1 {
		t.Fatalf("conflict created another landing: %v err=%v", landings, err)
	}
}

func TestDeclarationGuardKeepsAnAdmittedLandingTarget(t *testing.T) {
	canonical := t.TempDir()
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	crashMidApply(t, store, p, canonical)
	for _, desired := range [][]project.Project{{}, {{ID: p.ID, Home: project.Home{Path: t.TempDir()}}}, {{ID: "other", Home: p.Home}}, {{ID: p.ID, Home: p.Home}, {ID: "nested", Home: project.Home{Path: filepath.Join(canonical, "sub")}}}} {
		if err := store.ledger.Update(t.Context(), func(tx *ledger.Tx) error { return CheckDeclarationsTx(tx, desired) }); err == nil {
			t.Fatal("unfinished landing lost its target ownership")
		}
	}
	if err := store.ledger.Update(t.Context(), func(tx *ledger.Tx) error { return CheckDeclarationsTx(tx, []project.Project{p}) }); err != nil {
		t.Fatal(err)
	}
}

func TestApplyingRecoveryUsesHistoricalTargetButNeverAMovedDirectory(t *testing.T) {
	for _, move := range []bool{false, true} {
		canonical := t.TempDir()
		store, p := newStore(t, &localNode{}, project.Home{Path: canonical})
		land, _ := crashMidApply(t, store, p, canonical)
		if move {
			other := p
			other.Home.Path = t.TempDir()
			if err := store.projects.Declare(t.Context(), []project.Project{other}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.recoverLanding(context.Background(), land); err == nil {
				t.Fatal("recovery followed reassigned project metadata")
			}
		} else {
			if err := store.projects.Reconcile(t.Context(), nil, "test"); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := store.projects.Get(t.Context(), p.ID); ok {
				t.Fatal("project not retired")
			}
			recovered, err := store.recoverLanding(t.Context(), land)
			if err != nil || recovered.State != LandCommitted {
				t.Fatalf("historical recovery=%+v err=%v", recovered, err)
			}
		}
	}
}

// leaseClock is the ledger's clock under a driver test: the test moves it
// by hand and paces renewals through a channel, so no wall-clock ratio is
// left for a slow runner to break.
type leaseClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *leaseClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *leaseClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestLandingDriverRenewsAndRejectsStaleTransitions(t *testing.T) {
	canonical := t.TempDir()
	clock := &leaseClock{t: time.Now()}
	store, _ := newStoreWith(t, &localNode{}, project.Home{Path: canonical}, ledger.Options{Now: clock.now})
	ticks := make(chan time.Time)
	store.renewTicks = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	ttl := time.Minute
	lease, err := store.ledger.Acquire(t.Context(), "landing-driver:test", "first", ttl)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := store.startLandingDriver(t.Context(), lease, ttl)
	defer stop()
	land := Landing{ID: "test", State: LandProposed, Project: "p", Target: project.Home{Path: canonical}}
	if _, err := store.ledger.Begin(t.Context(), land.ID, landKind, LandProposed, "test", land); err != nil {
		t.Fatal(err)
	}
	// Two renewals past the halfway mark; the second tick is only taken
	// once the first renewal has committed, so the lease now outlives its
	// original expiry.
	clock.advance(ttl / 2)
	ticks <- time.Time{}
	ticks <- time.Time{}
	clock.advance(ttl/2 + ttl/6)
	if _, err := store.ledger.Acquire(t.Context(), lease.Key, "second", ttl); !errors.Is(err, ledger.ErrHeld) {
		t.Fatalf("landing driver expired during work: %v", err)
	}
	if err := store.ledger.Invalidate(t.Context(), lease.Key); err != nil {
		t.Fatal(err)
	}
	// The next renewal finds the lease gone; a renewal already racing the
	// invalidation has stopped the driver by itself.
	select {
	case ticks <- time.Time{}:
	case <-ctx.Done():
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("lost landing driver not cancelled")
	}
	if err := store.move(context.WithoutCancel(ctx), &land, LandProposed, LandLocked, nil); !errors.Is(err, ledger.ErrStale) {
		t.Fatalf("stale landing driver changed phase: %v", err)
	}
}
