package ledger

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// Two regions: east holds the ledger under test, west is another hub
// reached over HTTP. A lease issued by west is taken from east, fences an
// east transition through west's check, and stops matching the moment
// west says so.
func TestLeasesOfAnotherRegionAreIssuedAndCheckedThere(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	east := open(t, t.TempDir(), c)
	east.SetRegion("east")
	west := open(t, t.TempDir(), c)
	west.SetRegion("west")
	server := httptest.NewServer(IssuerHandler(west, "west-token"))
	defer server.Close()
	east.RegisterIssuer("west", NewHTTPIssuer(server.URL, "west-token"))
	ctx := context.Background()

	// Wrong token: refused. Unknown region: refused.
	east.RegisterIssuer("north", NewHTTPIssuer(server.URL, "wrong"))
	if _, err := east.AcquireIn(ctx, "north", "endpoint:n/x:slot:1", "a", time.Minute); err == nil {
		t.Fatal("a bad token acquired a lease")
	}
	if _, err := east.AcquireIn(ctx, "south", "k", "a", time.Minute); !errors.Is(err, ErrUnknownRegion) {
		t.Fatalf("unknown region = %v", err)
	}

	slot, err := east.AcquireIn(ctx, "west", "endpoint:node-w/codex:slot:1", "att-1", time.Minute)
	if err != nil || slot.Region != "west" || slot.Epoch != 1 {
		t.Fatalf("west lease = %+v err=%v", slot, err)
	}
	// It lives in west, not east.
	if _, ok, _ := east.LeaseOf(ctx, slot.Key); ok {
		t.Fatal("a west lease was written into east's ledger")
	}
	if l, ok, _ := west.LeaseOf(ctx, slot.Key); !ok || l.Holder != "att-1" {
		t.Fatalf("west does not hold it: %+v", l)
	}
	// An east operation fenced on it: west is asked.
	if _, err := east.Begin(ctx, "att-1", "attempt", "running", "hub", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := east.Transition(ctx, "att-1", "running", "bind-ready", "hub", []Lease{slot}, nil, nil); err != nil {
		t.Fatalf("transition fenced on a live west lease = %v", err)
	}
	renewed, err := east.RenewAny(ctx, slot, time.Minute)
	if err != nil || renewed.Region != "west" {
		t.Fatalf("renew = %+v err=%v", renewed, err)
	}
	// West cuts it (a takeover there); east's next fenced transition fails
	// with the same staleness the local case has.
	if err := west.Invalidate(ctx, slot.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := east.Transition(ctx, "att-1", "bind-ready", "bound", "hub", []Lease{slot}, nil, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("transition on a cut west lease = %v", err)
	}
	if err := east.ReleaseAny(ctx, slot); !errors.Is(err, ErrStale) {
		t.Fatalf("release of a cut west lease = %v", err)
	}
	// Local leases keep working unchanged.
	local, err := east.AcquireIn(ctx, "", "attempt:x", "x", time.Minute)
	if err != nil || local.Region != "east" {
		t.Fatalf("local lease = %+v err=%v", local, err)
	}
}
