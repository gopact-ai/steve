package intent

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestSameCallFromANewAttemptIsBlockedUntilResolved(t *testing.T) {
	l, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := New(l)
	ctx := context.Background()
	args := []byte(`{"content":"phase 1 done"}`)

	first, err := s.Claim(ctx, "t1", "att-1", "channel_send", args)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatched(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	// The hub loses the answer: outcome unknown.
	if err := s.Lost(ctx, first.ID, errors.New("timeout")); err != nil {
		t.Fatal(err)
	}
	// The next attempt of the same task asks for the same call.
	_, err = s.Claim(ctx, "t1", "att-2", "channel_send", args)
	var blocked Blocked
	if !errors.As(err, &blocked) || blocked.Previous.ID != first.ID {
		t.Fatalf("second attempt's call = %v", err)
	}
	// A different call, or another task, is not blocked.
	if _, err := s.Claim(ctx, "t1", "att-2", "channel_send", []byte(`{"content":"other"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, "t2", "att-3", "channel_send", args); err != nil {
		t.Fatal(err)
	}
	// A person says it happened: the record is done and the block lifts.
	if _, err := s.Resolve(ctx, first.ID, "maybe", "owner"); err == nil {
		t.Fatal("a made-up verdict was accepted")
	}
	resolved, err := s.Resolve(ctx, first.ID, "happened", "owner")
	if err != nil || resolved.State != Succeeded {
		t.Fatalf("resolve = %+v err=%v", resolved, err)
	}
	again, err := s.Claim(ctx, "t1", "att-2", "channel_send", args)
	if err != nil {
		t.Fatalf("still blocked after resolution: %v", err)
	}
	if err := s.Dispatched(ctx, again.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Confirmed(ctx, again.ID, map[string]string{"message_id": "om_1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, again.ID)
	if got.State != Succeeded || string(got.Receipt) == "" {
		t.Fatalf("confirmed = %+v", got)
	}
	// The journal saw the dispatch of both, and only one confirmation.
	outcomes, _ := l.Journal().Reconcile()
	known, unknown := 0, 0
	for _, o := range outcomes {
		if o.Effect.Kind != "dispatch" {
			continue
		}
		if o.Known() {
			known++
		} else {
			unknown++
		}
	}
	if known != 1 || unknown != 1 {
		t.Fatalf("journal known=%d unknown=%d", known, unknown)
	}
	unresolved, _ := s.Unresolved(ctx)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %+v", unresolved)
	}
}
