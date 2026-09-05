package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func open(t *testing.T, dir string, c *clock) *Ledger {
	t.Helper()
	l, err := Open(dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestCommandIsIdempotent(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	l := open(t, t.TempDir(), c)
	runs := 0
	run := func(context.Context) (json.RawMessage, error) {
		runs++
		return json.RawMessage(`{"ok":true}`), nil
	}
	first, replayed, err := l.Command(t.Context(), "msg-1", "chat", "user", run)
	if err != nil || replayed || string(first) != `{"ok":true}` {
		t.Fatalf("first = %s replayed=%v err=%v", first, replayed, err)
	}
	second, replayed, err := l.Command(t.Context(), "msg-1", "chat", "user", run)
	if err != nil || !replayed || string(second) != `{"ok":true}` {
		t.Fatalf("second = %s replayed=%v err=%v", second, replayed, err)
	}
	if runs != 1 {
		t.Fatalf("command ran %d times", runs)
	}
	// A failure is also an answer the retrying client gets back.
	_, _, err = l.Command(t.Context(), "msg-2", "chat", "user", func(context.Context) (json.RawMessage, error) {
		return nil, errors.New("boom")
	})
	if err == nil {
		t.Fatal("first run should have failed")
	}
	_, replayed, err = l.Command(t.Context(), "msg-2", "chat", "user", run)
	if !replayed || err == nil || err.Error() != "boom" {
		t.Fatalf("replayed failure = %v replayed=%v", err, replayed)
	}
	if runs != 1 {
		t.Fatal("a failed command was re-run")
	}
}

func TestLeaseIsExactMatchOrNothing(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	l := open(t, t.TempDir(), c)
	ctx := t.Context()

	a, err := l.Acquire(ctx, "canonical:steve", "attempt-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if a.Epoch != 1 || a.Incarnation != 1 {
		t.Fatalf("first lease = %+v", a)
	}
	// Held: someone else cannot take it.
	if _, err := l.Acquire(ctx, "canonical:steve", "attempt-2", time.Minute); !errors.Is(err, ErrHeld) {
		t.Fatalf("acquire while held = %v", err)
	}
	// Expired: the next holder gets a higher epoch, and the old lease is stale.
	c.advance(2 * time.Minute)
	b, err := l.Acquire(ctx, "canonical:steve", "attempt-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if b.Epoch != 2 {
		t.Fatalf("epoch after takeover = %d", b.Epoch)
	}
	if _, err := l.Renew(ctx, a, time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("stale renew = %v", err)
	}
	if err := l.Release(ctx, a); !errors.Is(err, ErrStale) {
		t.Fatalf("stale release = %v", err)
	}
	// A tampered tuple does not match either.
	forged := b
	forged.Holder = "attempt-9"
	if _, err := l.Renew(ctx, forged, time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("forged holder renew = %v", err)
	}
	forged = b
	forged.Incarnation = 7
	if _, err := l.Renew(ctx, forged, time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong incarnation renew = %v", err)
	}
	// The real one works and can be released; after release the resource is free.
	if _, err := l.Renew(ctx, b, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(ctx, "canonical:steve", "attempt-3", time.Minute); err != nil {
		t.Fatalf("acquire after release = %v", err)
	}
}

// Supersede must not touch a resource someone else has since acquired.
func TestInvalidateHeldByOnlyTouchesTheHoldersOwn(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	l := open(t, t.TempDir(), c)
	ctx := t.Context()
	old, _ := l.Acquire(ctx, "attempt:old", "old", time.Minute)
	slot, _ := l.Acquire(ctx, "endpoint:e1:slot:1", "old", time.Minute)
	c.advance(2 * time.Minute)
	// The slot has legitimately moved on to another attempt.
	other, err := l.Acquire(ctx, "endpoint:e1:slot:1", "new", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := l.InvalidateHeldBy(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "attempt:old" {
		t.Fatalf("invalidated %v, want only the attempt key", keys)
	}
	if _, err := l.Renew(ctx, old, time.Minute); !errors.Is(err, ErrStale) {
		t.Fatal("the old attempt lease survived supersede")
	}
	if _, err := l.Renew(ctx, other, time.Minute); err != nil {
		t.Fatalf("the successor's slot lease was damaged: %v", err)
	}
	_ = slot
}

func TestTransitionIsAtomicWithNameCASAndFencing(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	l := open(t, t.TempDir(), c)
	ctx := t.Context()

	type payload struct{ Note string }
	if _, err := l.Begin(ctx, "att-1", "attempt", "bind-ready", "hub", payload{"x"}); err != nil {
		t.Fatal(err)
	}
	lease, _ := l.Acquire(ctx, "attempt:att-1", "att-1", time.Minute)

	// Wrong from-state is a conflict and writes nothing.
	if _, err := l.Transition(ctx, "att-1", "running", "bound", "hub", nil, nil, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong from = %v", err)
	}
	// A stale fencing refuses the whole transition, mutation included.
	stale := lease
	stale.Epoch = 99
	_, err := l.Transition(ctx, "att-1", "bind-ready", "bound", "hub", []Lease{stale}, nil, func(tx *Tx, op *Operation) error {
		_, err := tx.CompareAndSetName("steve/1/build", 0, "sha-a")
		return err
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("stale fencing = %v", err)
	}
	if _, ok, _ := l.Name(ctx, "steve/1/build"); ok {
		t.Fatal("the name was bound despite the refused transition")
	}
	// Good fencing: name CAS, data update and event land together.
	ev, err := l.Transition(ctx, "att-1", "bind-ready", "bound", "hub", []Lease{lease}, map[string]string{"ref": "steve/1/build"},
		func(tx *Tx, op *Operation) error {
			if _, err := tx.CompareAndSetName("steve/1/build", 0, "sha-a"); err != nil {
				return err
			}
			return tx.SetData(op, payload{"bound"})
		})
	if err != nil {
		t.Fatal(err)
	}
	if ev.From != "bind-ready" || ev.To != "bound" || ev.Revision != 2 || len(ev.Fencings) != 1 {
		t.Fatalf("event = %+v", ev)
	}
	ref, ok, _ := l.Name(ctx, "steve/1/build")
	if !ok || ref.Version != 1 || ref.Artifact != "sha-a" {
		t.Fatalf("ref = %+v", ref)
	}
	op, _, _ := l.Operation(ctx, "att-1")
	if op.State != "bound" || !strings.Contains(string(op.Data), "bound") {
		t.Fatalf("op = %+v", op)
	}
	// A second bind against the same name at the wrong version loses.
	if _, err := l.Begin(ctx, "att-2", "attempt", "bind-ready", "hub", nil); err != nil {
		t.Fatal(err)
	}
	_, err = l.Transition(ctx, "att-2", "bind-ready", "bound", "hub", nil, nil, func(tx *Tx, op *Operation) error {
		_, err := tx.CompareAndSetName("steve/1/build", 0, "sha-b")
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale name version = %v", err)
	}
	if op, _, _ := l.Operation(ctx, "att-2"); op.State != "bind-ready" {
		t.Fatalf("losing bind moved the operation to %s", op.State)
	}
	events, _ := l.Events(ctx, "att-1")
	if len(events) != 2 || events[0].From != "" || events[1].To != "bound" {
		t.Fatalf("history = %+v", events)
	}
}

func TestIncarnationRotationInvalidatesEverythingAndRequiresRecovery(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	l := open(t, dir, c)
	ctx := t.Context()
	lease, _ := l.Acquire(ctx, "attempt:a", "a", time.Hour)
	if l.Incarnation() != 1 {
		t.Fatalf("incarnation = %d", l.Incarnation())
	}
	l.Close()

	// Restore-from-backup looks like: incarnation file rotated, database older.
	if _, err := Rotate(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{Now: c.now}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("open after rotate without recover = %v", err)
	}
	l2, err := Open(dir, Options{Now: c.now, Recover: true})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if !l2.InRecovery() || l2.Incarnation() != 2 {
		t.Fatalf("recovery=%v incarnation=%d", l2.InRecovery(), l2.Incarnation())
	}
	if err := l2.InvalidateAll(ctx); err != nil {
		t.Fatal(err)
	}
	// The pre-restore lease matches nothing now, even if unexpired.
	if _, err := l2.Renew(ctx, lease, time.Hour); !errors.Is(err, ErrStale) {
		t.Fatalf("pre-restore lease renew = %v", err)
	}
	// A lost incarnation file is refused, not guessed at.
	l2.Close()
	if err := os.WriteFile(filepath.Join(dir, incarnationFile), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{Now: c.now}); !errors.Is(err, ErrIncarnationLost) {
		t.Fatalf("older incarnation file = %v", err)
	}
}

func TestEffectsJournalOnlyConfirmedCountsAsHappened(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	l := open(t, dir, c)
	j := l.Journal()
	msg := EffectID{Operation: "att-1", Kind: "message", InstanceKey: "occ-1"}
	path1 := EffectID{Operation: "land-1", Kind: "land-path", InstanceKey: "1/calc.go"}
	path2 := EffectID{Operation: "land-1", Kind: "land-path", InstanceKey: "2/calc.go"} // next WAL round
	if _, err := j.Started(msg, "cmd-1", map[string]string{"to": "chat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Confirmed(msg, map[string]string{"message_id": "om_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Started(path1, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Confirmed(path1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Started(path2, "", nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-line: a torn tail must not poison recovery.
	f, _ := os.OpenFile(filepath.Join(dir, journalFile), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"seq":99,"effect":{"operation":"x"`)
	f.Close()
	l.Close()

	l2 := open(t, dir, c)
	outcomes, err := l2.Journal().Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Outcome{}
	for _, o := range outcomes {
		byID[o.Effect.String()] = o
	}
	if !byID[msg.String()].Known() {
		t.Fatal("a confirmed message was not known to have happened")
	}
	if !byID[path1.String()].Known() {
		t.Fatal("round-1 path confirmation lost")
	}
	if byID[path2.String()].Known() {
		t.Fatal("a round-2 path with only a start was treated as done — the round-1 confirmation leaked across rounds")
	}
	// New writes continue the sequence past the torn line.
	e, err := l2.Journal().Started(EffectID{Operation: "att-2", Kind: "message", InstanceKey: "occ-2"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq <= 5 {
		t.Fatalf("sequence did not continue: %d", e.Seq)
	}
}

func TestBindingsRoundTrip(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	l := open(t, t.TempDir(), c)
	type home struct{ Node, Path string }
	if err := l.PutBinding(t.Context(), "project-home", "steve", home{"host-1", "/w"}); err != nil {
		t.Fatal(err)
	}
	var got home
	ok, err := l.GetBinding(t.Context(), "project-home", "steve", &got)
	if err != nil || !ok || got.Node != "host-1" {
		t.Fatalf("binding = %+v ok=%v err=%v", got, ok, err)
	}
	all, _ := l.Bindings(t.Context(), "project-home")
	if len(all) != 1 {
		t.Fatalf("bindings = %v", all)
	}
}

// A command still running is not run again by a retry that arrives
// meanwhile: the second caller is told it is in flight, and gets the one
// answer once it exists.
func TestCommandInFlightIsNotRunTwice(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	l := open(t, t.TempDir(), c)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := l.Command(t.Context(), "msg-9", "chat", "user", func(context.Context) (json.RawMessage, error) {
			close(started)
			<-release
			return json.RawMessage(`{"n":1}`), nil
		})
		done <- err
	}()
	<-started
	if _, replayed, err := l.Command(t.Context(), "msg-9", "chat", "user", func(context.Context) (json.RawMessage, error) {
		t.Fatal("the in-flight command was run a second time")
		return nil, nil
	}); !replayed || !errors.Is(err, ErrInFlight) {
		t.Fatalf("concurrent retry = replayed:%v err:%v", replayed, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out, replayed, err := l.Command(t.Context(), "msg-9", "chat", "user", nil)
	if err != nil || !replayed || string(out) != `{"n":1}` {
		t.Fatalf("after completion = %s replayed=%v err=%v", out, replayed, err)
	}
}
