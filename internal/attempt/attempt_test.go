package attempt

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newService(t *testing.T) (*Service, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	l, err := ledger.Open(t.TempDir(), ledger.Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	s := New(l)
	s.now = c.now
	s.TTL = time.Minute
	return s, c
}

func canonical(projectID string) project.Workspace {
	return project.Workspace{ID: "canonical:" + projectID, Project: projectID, Path: "/w/" + projectID, Kind: project.KindCanonical}
}

func worktree(id, projectID string) project.Workspace {
	return project.Workspace{ID: id, Project: projectID, Path: "/wt/" + id, Kind: project.KindWorktree}
}

func TestInPlaceTurnsSerializeOnTheCanonicalLock(t *testing.T) {
	s, c := newService(t)
	ctx := context.Background()
	first, err := s.Open(ctx, Spec{TaskID: "1", Kind: KindChat, Project: "p", Agent: "codex", Harness: "codex", Workspace: canonical("p"), Scope: ScopeUnrestricted})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Leases) != 2 || first.Leases[1].Key != "canonical:p" {
		t.Fatalf("leases = %+v", first.Leases)
	}
	_, err = s.Open(ctx, Spec{TaskID: "2", Kind: KindChat, Project: "p", Agent: "claude", Harness: "claude", Workspace: canonical("p"), Scope: ScopeUnrestricted})
	var busy Busy
	if !errors.As(err, &busy) || busy.Resource != "canonical:p" || busy.Holder != first.ID {
		t.Fatalf("second in-place open = %v", err)
	}
	// The refused open left nothing behind.
	live, _ := s.Live(ctx)
	if len(live) != 1 {
		t.Fatalf("live = %d", len(live))
	}
	// Another project is untouched.
	if _, err := s.Open(ctx, Spec{TaskID: "3", Kind: KindChat, Project: "q", Workspace: canonical("q"), Scope: ScopeUnrestricted}); err != nil {
		t.Fatal(err)
	}
	// Finishing releases the lock.
	if _, err := s.Advance(ctx, first.ID, Prepared, "hub", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(ctx, first.ID, Running, "hub", nil); err != nil {
		t.Fatal(err)
	}
	bound, err := s.Finish(ctx, first.ID, "hub", Result{Summary: "done"})
	if err != nil || bound.State != Bound || bound.Result.Summary != "done" || bound.EndedAt.IsZero() {
		t.Fatalf("finish = %+v err=%v", bound, err)
	}
	if _, err := s.Open(ctx, Spec{TaskID: "2", Kind: KindChat, Project: "p", Workspace: canonical("p"), Scope: ScopeUnrestricted}); err != nil {
		t.Fatalf("open after release = %v", err)
	}
	// History: leased → prepared → running → bind-ready → bound, fenced.
	events, _ := s.History(ctx, first.ID)
	if len(events) != 5 || events[4].To != "bound" || len(events[4].Fencings) != 2 {
		t.Fatalf("events = %+v", events)
	}
	_ = c
}

func TestScopeIsBoundToWorkspaceKind(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	if _, err := s.Open(ctx, Spec{Kind: KindStep, Project: "p", Workspace: canonical("p"), Scope: ScopePathSet}); err == nil {
		t.Fatal("path-set on the canonical workspace was allowed")
	}
	if _, err := s.Open(ctx, Spec{Kind: KindChat, Project: "p", Workspace: worktree("wt1", "p"), Scope: ScopeUnrestricted}); err == nil {
		t.Fatal("unrestricted in a worktree was allowed")
	}
	if _, err := s.Open(ctx, Spec{Kind: KindStep, Project: "p", Workspace: worktree("wt1", "p")}); err == nil {
		t.Fatal("an attempt without a scope was allowed")
	}
	// Two isolated steps on one project run side by side; the same
	// worktree does not.
	a, err := s.Open(ctx, Spec{Kind: KindStep, Project: "p", Workspace: worktree("wt1", "p"), Scope: ScopePathSet, Touches: []string{"a/"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, Spec{Kind: KindStep, Project: "p", Workspace: worktree("wt2", "p"), Scope: ScopePathSet}); err != nil {
		t.Fatalf("parallel isolated step = %v", err)
	}
	if _, err := s.Open(ctx, Spec{Kind: KindStep, Project: "p", Workspace: worktree("wt1", "p"), Scope: ScopeNone}); err == nil {
		t.Fatal("two attempts in one worktree were allowed")
	}
	_ = a
}

func TestEndpointSlotsAreLeased(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	open := func(id string) (Record, error) {
		return s.Open(ctx, Spec{ID: id, Kind: KindStep, Project: "p", Node: "node-a", Harness: "codex", Slots: 2,
			Workspace: worktree("wt-"+id, "p"), Scope: ScopePathSet})
	}
	if _, err := open("a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := open("a2"); err != nil {
		t.Fatal(err)
	}
	_, err := open("a3")
	var full NoSlot
	if !errors.As(err, &full) || full.Endpoint != "endpoint:node-a/codex" {
		t.Fatalf("third open = %v", err)
	}
	if _, err := s.Fail(ctx, "a1", "hub", "boom"); err != nil {
		t.Fatal(err)
	}
	if _, err := open("a3"); err != nil {
		t.Fatalf("open after a slot freed = %v", err)
	}
}

func TestLostLeaseStopsEveryTransition(t *testing.T) {
	s, c := newService(t)
	ctx := context.Background()
	r, _ := s.Open(ctx, Spec{Kind: KindStep, Project: "p", Workspace: worktree("wt1", "p"), Scope: ScopePathSet})
	if _, err := s.Advance(ctx, r.ID, Prepared, "hub", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(ctx, r.ID, Running, "hub", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	// Illegal edge.
	if _, err := s.Advance(ctx, r.ID, Bound, "hub", nil); !errors.Is(err, ErrBadState) {
		t.Fatalf("running → bound = %v", err)
	}
	// A lost running lease proves no process exit: quarantine the writer.
	c.t = c.t.Add(2 * time.Minute)
	expired, err := s.Sweep(ctx)
	if err != nil || len(expired) != 0 {
		t.Fatalf("sweep = %+v err=%v", expired, err)
	}
	if err := s.Renew(ctx, r.ID); !errors.Is(err, ErrLost) {
		t.Fatalf("renew after expiry = %v", err)
	}
	// The zombie driver cannot complete while its writer remains unknown.
	if _, err := s.Finish(ctx, r.ID, "zombie", Result{Summary: "late"}); err == nil {
		t.Fatal("a zombie finished an expired attempt")
	}
	got, _ := s.Get(ctx, r.ID)
	if got.State != Running || !got.Unsettled || got.Result != nil {
		t.Fatalf("record = %+v", got)
	}
}

func TestSupersedeCutsOnlyTheOldAttemptsLeases(t *testing.T) {
	s, c := newService(t)
	ctx := context.Background()
	old, _ := s.Open(ctx, Spec{ID: "old", Kind: KindStep, Project: "p", Node: "node-a", Harness: "codex", Slots: 1,
		Workspace: worktree("wt-old", "p"), Scope: ScopePathSet, Base: "steve/1/build"})
	_, _ = s.Advance(ctx, "old", Prepared, "hub", nil)
	_, _ = s.Advance(ctx, "old", Running, "hub", nil)
	// In place attempts are never taken over.
	chat, _ := s.Open(ctx, Spec{Kind: KindChat, Project: "q", Workspace: canonical("q"), Scope: ScopeUnrestricted})
	if _, err := s.Supersede(ctx, chat.ID, Spec{Kind: KindChat, Project: "q", Workspace: canonical("q"), Scope: ScopeUnrestricted}, "hub"); err == nil {
		t.Fatal("an in-place attempt was taken over")
	}
	if err := s.MarkSessionSettled(ctx, "old", "worker confirmed finished"); err != nil {
		t.Fatal(err)
	}
	// Its session is confirmed finished; the slot it held has expired
	// and been legitimately re-leased by someone else before takeover.
	c.t = c.t.Add(2 * time.Minute)
	other, err := s.l.Acquire(ctx, "endpoint:node-a/codex:slot:1", "other", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Supersede(ctx, "old", Spec{Kind: KindStep, Project: "p", Node: "node-b", Harness: "codex",
		Workspace: worktree("wt-new", "p"), Scope: ScopePathSet}, "hub")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Base != "steve/1/build" {
		t.Fatalf("base not carried: %+v", fresh)
	}
	gone, _ := s.Get(ctx, "old")
	if gone.State != Superseded || gone.SupersededBy != fresh.ID {
		t.Fatalf("old = %+v", gone)
	}
	// The other holder's slot lease survived; the old attempt's own did not.
	if _, err := s.l.Renew(ctx, other, time.Minute); err != nil {
		t.Fatalf("someone else's slot lease was cut: %v", err)
	}
	if _, err := s.l.Renew(ctx, old.Leases[0], time.Minute); !errors.Is(err, ledger.ErrStale) {
		t.Fatal("the old attempt lease survived takeover")
	}
	if err := s.Renew(ctx, "old"); !errors.Is(err, ErrLost) {
		t.Fatal("the old attempt can still renew")
	}
}

func TestHeartbeatReportsLoss(t *testing.T) {
	s, _ := newService(t)
	s.TTL = 90 * time.Millisecond
	s.now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := s.Open(ctx, Spec{Kind: KindStep, Project: "p", Workspace: worktree("wt", "p"), Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	lost := s.Heartbeat(ctx, r.ID)
	select {
	case <-lost:
		t.Fatal("lost before anything happened")
	case <-time.After(200 * time.Millisecond):
	}
	// Someone cuts the lease underneath; the next beat reports it.
	if err := s.l.Invalidate(ctx, "attempt:"+r.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat never noticed the lost lease")
	}
}

func TestAReservedSlotWaitsForItsAttempt(t *testing.T) {
	s, c := newService(t)
	ctx := context.Background()
	// One slot on node-a/codex, reserved for a plan step.
	r, err := s.Reserve(ctx, "resv-1", "node-a", "codex", 1, "plan 1/build", "supervisor", time.Minute)
	if err != nil || r.Endpoint != "endpoint:node-a/codex" {
		t.Fatalf("reserve = %+v err=%v", r, err)
	}
	// Nobody else gets the slot meanwhile.
	_, err = s.Open(ctx, Spec{ID: "other", Kind: KindStep, Project: "p", Node: "node-a", Harness: "codex", Slots: 1, Workspace: worktree("wt-o", "p"), Scope: ScopePathSet})
	var full NoSlot
	if !errors.As(err, &full) {
		t.Fatalf("open while reserved = %v", err)
	}
	if _, err := s.Reserve(ctx, "resv-2", "node-a", "codex", 1, "plan 1/other", "supervisor", time.Minute); !errors.As(err, &full) {
		t.Fatalf("second reservation = %v", err)
	}
	// The attempt made for it takes the slot over under a new epoch.
	rec, err := s.Open(ctx, Spec{ID: "a1", Kind: KindStep, Project: "p", Node: "node-a", Harness: "codex", Slots: 1, Reservation: "resv-1", Workspace: worktree("wt-1", "p"), Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Leases) != 3 || rec.Leases[2].Holder != "a1" || rec.Leases[2].Epoch != r.Lease.Epoch+1 {
		t.Fatalf("leases after takeover = %+v", rec.Leases)
	}
	if _, ok, _ := s.Reservation(ctx, "resv-1"); ok {
		t.Fatal("the reservation survived being taken")
	}
	if _, err := s.l.Renew(ctx, r.Lease, time.Minute); !errors.Is(err, ledger.ErrStale) {
		t.Fatal("the reservation's own lease still matches after transfer")
	}
	// A reservation nobody takes expires and frees the slot.
	r2, _ := s.Reserve(ctx, "resv-3", "node-b", "codex", 1, "x", "supervisor", time.Minute)
	c.t = c.t.Add(2 * time.Minute)
	if _, err := s.Open(ctx, Spec{ID: "b1", Kind: KindStep, Project: "p", Node: "node-b", Harness: "codex", Slots: 1, Workspace: worktree("wt-b", "p"), Scope: ScopePathSet}); err != nil {
		t.Fatalf("open after the reservation expired = %v", err)
	}
	if _, err := s.Open(ctx, Spec{ID: "b2", Kind: KindStep, Project: "p", Node: "node-b", Harness: "codex", Slots: 1, Reservation: r2.ID, Workspace: worktree("wt-b2", "p"), Scope: ScopePathSet}); err == nil {
		t.Fatal("an expired reservation was taken over")
	}
}

func TestAttemptLeasesAreIssuedByTheMachinesRegion(t *testing.T) {
	s, c := newService(t)
	west, err := ledger.Open(t.TempDir(), ledger.Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	defer west.Close()
	west.SetRegion("west")
	s.l.SetRegion("east")
	s.l.RegisterIssuer("west", west) // an in-process issuer: same contract as the HTTP one
	ctx := context.Background()
	rec, err := s.Open(ctx, Spec{ID: "a1", Kind: KindStep, Project: "p", Node: "node-w", Harness: "codex", Slots: 1, Region: "west",
		Workspace: worktree("wt-w", "p"), Scope: ScopePathSet})
	if err != nil {
		t.Fatal(err)
	}
	regions := map[string]string{}
	for _, l := range rec.Leases {
		regions[l.Key] = l.Region
	}
	if regions["attempt:a1"] != "east" || regions["workspace:wt-w"] != "west" || regions["endpoint:node-w/codex:slot:1"] != "west" {
		t.Fatalf("lease regions = %v", regions)
	}
	if _, ok, _ := west.LeaseOf(ctx, "workspace:wt-w"); !ok {
		t.Fatal("the workspace lease is not in west's ledger")
	}
	if _, ok, _ := s.l.LeaseOf(ctx, "workspace:wt-w"); ok {
		t.Fatal("the workspace lease leaked into east's ledger")
	}
	if err := s.Renew(ctx, "a1"); err != nil {
		t.Fatalf("renew across regions: %v", err)
	}
	// West cuts the slot: the attempt is lost, exactly as with a local lease.
	if err := west.Invalidate(ctx, "endpoint:node-w/codex:slot:1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(ctx, "a1"); !errors.Is(err, ErrLost) {
		t.Fatalf("renew after west cut the slot = %v", err)
	}
	if _, err := s.Advance(ctx, "a1", Prepared, "hub", nil); !errors.Is(err, ErrLost) {
		t.Fatalf("advance after west cut the slot = %v", err)
	}
	// An unknown region is refused before anything is leased.
	if _, err := s.Open(ctx, Spec{ID: "a2", Kind: KindStep, Project: "p", Node: "node-s", Harness: "codex", Region: "south",
		Workspace: worktree("wt-s", "p"), Scope: ScopePathSet}); !errors.Is(err, ledger.ErrUnknownRegion) {
		t.Fatalf("unknown region = %v", err)
	}
	if live, _ := s.Live(ctx); len(live) != 1 {
		t.Fatalf("live = %d", len(live))
	}
}

func TestExpireAllFreesWhatTheDeadProcessHeld(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	held, err := s.Open(ctx, Spec{TaskID: "1", Kind: KindChat, Project: "p", Agent: "claude", Harness: "claude", Workspace: canonical("p"), Scope: ScopeUnrestricted})
	if err != nil {
		t.Fatal(err)
	}
	// The lease is fresh, so the periodic sweep leaves it: only the
	// process's own death says it is over.
	if expired, err := s.Sweep(ctx); err != nil || len(expired) != 0 {
		t.Fatalf("sweep = %v, %v", expired, err)
	}
	expired, err := s.ExpireAll(ctx, "hub restarted")
	if err != nil || len(expired) != 1 || expired[0].ID != held.ID || expired[0].State != Expired {
		t.Fatalf("expire all = %+v, %v", expired, err)
	}
	if live, _ := s.Live(ctx); len(live) != 0 {
		t.Fatalf("still live: %+v", live)
	}
	// The same task opens again on the same workspace: nothing holds it.
	again, err := s.Open(ctx, Spec{TaskID: "1", Kind: KindChat, Project: "p", Agent: "claude", Harness: "claude", Workspace: canonical("p"), Scope: ScopeUnrestricted})
	if err != nil || again.ID == held.ID {
		t.Fatalf("reopen = %+v, %v", again, err)
	}
	if got, _ := s.Get(ctx, held.ID); got.State != Expired || got.Error != "hub restarted" {
		t.Fatalf("old attempt = %+v", got)
	}
}
