package task

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	clock := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	return store, &clock
}

func mustCreate(t *testing.T, s *Store, goal, channel string) Task {
	t.Helper()
	created, err := s.Create(Task{Goal: goal, Channel: channel})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return created
}

func TestCreateAssignsSequentialIDsAndDefaults(t *testing.T) {
	store, _ := newStore(t)
	first := mustCreate(t, store, "wire the node link", "chat-a")
	second := mustCreate(t, store, "review the diff", "chat-a")

	if first.ID != "1" || second.ID != "2" {
		t.Fatalf("ids = %q, %q; want 1, 2", first.ID, second.ID)
	}
	if first.State != StateDraft {
		t.Fatalf("state = %q; want draft", first.State)
	}
	if first.Budget.MaxTurns != DefaultMaxTurns || first.Budget.MaxElapsed != DefaultMaxElapsed {
		t.Fatalf("budget defaults not applied: %+v", first.Budget)
	}
}

func TestBeginFinishAccumulatesBudget(t *testing.T) {
	store, clock := newStore(t)
	created := mustCreate(t, store, "wire the node link", "chat-a")

	running, err := store.Begin(created.ID, "builder", "laptop", "sess-1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if running.State != StateRunning || running.Budget.Turns != 1 {
		t.Fatalf("after begin: state=%q turns=%d", running.State, running.Budget.Turns)
	}
	if len(running.Attempts) != 1 || !running.Attempts[0].Open() {
		t.Fatalf("expected one open attempt, got %+v", running.Attempts)
	}

	*clock = clock.Add(90 * time.Second)
	done, err := store.Finish(created.ID, OutcomeOK, Tokens{Input: 10, Output: 5, Total: 15}, 3)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if done.Budget.Elapsed != 90*time.Second {
		t.Fatalf("elapsed = %v; want 90s", done.Budget.Elapsed)
	}
	if done.Budget.ToolCalls != 3 || done.Budget.Tokens.Total != 15 {
		t.Fatalf("budget = %+v", done.Budget)
	}
	if done.Attempts[0].Open() || done.Attempts[0].Outcome != OutcomeOK {
		t.Fatalf("attempt not closed: %+v", done.Attempts[0])
	}
}

func TestFinishTwiceIsRejected(t *testing.T) {
	store, _ := newStore(t)
	created := mustCreate(t, store, "goal", "chat-a")
	if _, err := store.Begin(created.ID, "builder", "laptop", ""); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.Finish(created.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := store.Finish(created.ID, OutcomeOK, Tokens{}, 0); err == nil {
		t.Fatal("second finish should fail")
	}
}

func TestBeginRefusesExhaustedTurnBudget(t *testing.T) {
	store, _ := newStore(t)
	created, err := store.Create(Task{Goal: "loop", Channel: "chat-a", Budget: Budget{MaxTurns: 1}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Begin(created.ID, "builder", "laptop", ""); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, err := store.Finish(created.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	_, err = store.Begin(created.ID, "builder", "laptop", "")
	if err == nil {
		t.Fatal("begin past the turn budget should fail")
	}
}

func TestBeginRefusesExhaustedElapsedBudget(t *testing.T) {
	store, clock := newStore(t)
	created, err := store.Create(Task{Goal: "slow", Channel: "chat-a", Budget: Budget{MaxElapsed: time.Minute}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Begin(created.ID, "builder", "laptop", ""); err != nil {
		t.Fatalf("begin: %v", err)
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := store.Finish(created.ID, OutcomeTimeout, Tokens{}, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := store.Begin(created.ID, "builder", "laptop", ""); err == nil {
		t.Fatal("begin past the elapsed budget should fail")
	}
}

func TestAdvanceEnforcesTransitions(t *testing.T) {
	store, _ := newStore(t)
	created := mustCreate(t, store, "goal", "chat-a")

	if _, err := store.Advance(created.ID, StateReview); err == nil {
		t.Fatal("draft -> review should be rejected")
	}
	if _, err := store.Begin(created.ID, "builder", "laptop", ""); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.Finish(created.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := store.Advance(created.ID, StateReview); err != nil {
		t.Fatalf("running -> review: %v", err)
	}
	done, err := store.Advance(created.ID, StateDone)
	if err != nil {
		t.Fatalf("review -> done: %v", err)
	}
	if !done.State.Terminal() {
		t.Fatal("done should be terminal")
	}
	if _, err := store.Advance(created.ID, StateRunning); err == nil {
		t.Fatal("done -> running should be rejected")
	}
}

func TestListIsNewestFirstAndScopedToChannel(t *testing.T) {
	store, clock := newStore(t)
	first := mustCreate(t, store, "a", "chat-a")
	*clock = clock.Add(time.Minute)
	second := mustCreate(t, store, "b", "chat-a")
	*clock = clock.Add(time.Minute)
	mustCreate(t, store, "c", "chat-b")

	scoped := store.List("chat-a")
	if len(scoped) != 2 {
		t.Fatalf("scoped list = %d; want 2", len(scoped))
	}
	if scoped[0].ID != second.ID || scoped[1].ID != first.ID {
		t.Fatalf("order = %q, %q; want newest first", scoped[0].ID, scoped[1].ID)
	}
	if len(store.List("")) != 3 {
		t.Fatalf("unscoped list = %d; want 3", len(store.List("")))
	}
}

func TestReopenRestoresTasksAndIDCounter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created := mustCreate(t, store, "survive a restart", "chat-a")
	if _, err := store.Begin(created.ID, "builder", "laptop", "sess-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	restored, ok := reopened.Get(created.ID)
	if !ok {
		t.Fatal("task missing after reopen")
	}
	if restored.State != StateRunning || len(restored.Attempts) != 1 {
		t.Fatalf("restored = %+v", restored)
	}
	next := mustCreate(t, reopened, "next", "chat-a")
	if next.ID != "2" {
		t.Fatalf("id after reopen = %q; want 2", next.ID)
	}
}

func TestReturnedTasksAreCopies(t *testing.T) {
	store, _ := newStore(t)
	created := mustCreate(t, store, "goal", "chat-a")
	if _, err := store.Begin(created.ID, "builder", "laptop", ""); err != nil {
		t.Fatalf("begin: %v", err)
	}

	got, _ := store.Get(created.ID)
	got.Goal = "mutated"
	got.Attempts[0].Member = "someone-else"

	again, _ := store.Get(created.ID)
	if again.Goal != "goal" || again.Attempts[0].Member != "builder" {
		t.Fatalf("store mutated through returned copy: %+v", again)
	}
}

func TestActiveIgnoresFinishedAndOtherMembers(t *testing.T) {
	store, clock := newStore(t)
	if _, ok := store.Active("chat-a", "builder", ""); ok {
		t.Fatal("no task yet, Active should miss")
	}

	first, err := store.Create(Task{Goal: "a", Channel: "chat-a", Member: "builder"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	*clock = clock.Add(time.Minute)
	if _, err := store.Create(Task{Goal: "other member", Channel: "chat-a", Member: "reviewer"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, ok := store.Active("chat-a", "builder", "")
	if !ok || got.ID != first.ID {
		t.Fatalf("Active = %+v, ok=%v; want task %s", got, ok, first.ID)
	}
	if _, err := store.Advance(first.ID, StateDone); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, ok := store.Active("chat-a", "builder", ""); ok {
		t.Fatal("finished task should not be active")
	}
}

func TestActivePrefersNewest(t *testing.T) {
	store, clock := newStore(t)
	if _, err := store.Create(Task{Goal: "old", Channel: "chat-a", Member: "builder"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	*clock = clock.Add(time.Minute)
	newer, err := store.Create(Task{Goal: "new", Channel: "chat-a", Member: "builder"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, ok := store.Active("chat-a", "builder", "")
	if !ok || got.ID != newer.ID {
		t.Fatalf("Active = %q; want %q", got.ID, newer.ID)
	}
}

func TestInterruptedListsOpenAttemptsAndAnchors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(Task{Goal: "long job", Channel: "chat", Member: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin(created.ID, "codex", "node", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAnchor(created.ID, "oc_1", "om_1", "group", "om_card_0"); err != nil {
		t.Fatal(err)
	}
	interrupted := store.Interrupted()
	if len(interrupted) != 1 || interrupted[0].ID != created.ID {
		t.Fatalf("interrupted = %+v", interrupted)
	}
	if interrupted[0].AnchorMessage != "om_1" || interrupted[0].ChatID != "oc_1" || interrupted[0].ChatType != "group" {
		t.Fatalf("anchor not persisted: %+v", interrupted[0])
	}
	if _, err := store.Finish(created.ID, OutcomeInterrupted, Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if got := store.Interrupted(); len(got) != 0 {
		t.Fatalf("closed attempt still interrupted: %+v", got)
	}
	// The record survives a reopen — that is the whole point.
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := reopened.Get(created.ID)
	if !ok || loaded.AnchorMessage != "om_1" || loaded.Attempts[0].Outcome != OutcomeInterrupted {
		t.Fatalf("reopen lost state: %+v", loaded)
	}
}

func TestSetBudgetRaisesDefaults(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetBudget(100, 6*time.Hour)
	created, err := store.Create(Task{Goal: "g", Channel: "c", Member: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Budget.MaxTurns != 100 || created.Budget.MaxElapsed != 6*time.Hour {
		t.Fatalf("budget = %+v", created.Budget)
	}
	store.SetBudget(0, 0) // non-positive keeps current
	again, _ := store.Create(Task{Goal: "g2", Channel: "c", Member: "m"})
	if again.Budget.MaxTurns != 100 {
		t.Fatalf("zero overwrote the default: %+v", again.Budget)
	}
}

func TestInterimJournalFollowsTheTurn(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, _ := store.Create(Task{Goal: "g", Channel: "chat", Member: "codex"})
	if _, err := store.Begin(created.ID, "codex", "n", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAnchor(created.ID, "oc_1", "om_1", "group", "om_card_1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddInterim("chat", "codex", "om_i1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddInterim("chat", "codex", "om_i2"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(created.ID)
	if got.OpenCard != "om_card_1" || len(got.Interim) != 2 || got.Interim[1] != "om_i2" {
		t.Fatalf("journal = %+v", got)
	}
	// The next turn wipes the slate: last turn's leftovers are not stale.
	if err := store.SetAnchor(created.ID, "oc_1", "om_2", "group", "om_card_2"); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(created.ID)
	if got.OpenCard != "om_card_2" || len(got.Interim) != 0 {
		t.Fatalf("new turn kept old leftovers: %+v", got)
	}
	if err := store.AddInterim("chat", "nobody", "om_x"); err == nil {
		t.Fatal("journal accepted a memberless conversation")
	}
}

// Unattended work and what a person typed are separate lineages. Without this
// a nightly schedule would charge its runs to whatever the user happened to be
// doing in the same chat — and a rotation would then close their task.
func TestOriginPartitionsTheActiveTask(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mine, err := store.Create(Task{Channel: "chat", Member: "codex", Goal: "my own thing"})
	if err != nil {
		t.Fatalf("create mine: %v", err)
	}
	nightly, err := store.Create(Task{Channel: "chat", Member: "codex", Goal: "nightly", Origin: "schedule:1"})
	if err != nil {
		t.Fatalf("create nightly: %v", err)
	}

	if got, ok := store.Active("chat", "codex", ""); !ok || got.ID != mine.ID {
		t.Fatalf("Active for a person = %+v, %t; want %s", got, ok, mine.ID)
	}
	if got, ok := store.Active("chat", "codex", "schedule:1"); !ok || got.ID != nightly.ID {
		t.Fatalf("Active for the schedule = %+v, %t; want %s", got, ok, nightly.ID)
	}
	if _, ok := store.Active("chat", "codex", "schedule:2"); ok {
		t.Fatal("a schedule with no task of its own borrowed another lineage's")
	}

	// The interim journal follows the turn that is actually running, not
	// whichever lineage was touched most recently.
	if _, err := store.Begin(mine.ID, "codex", "laptop", ""); err != nil {
		t.Fatalf("begin mine: %v", err)
	}
	if err := store.AddInterim("chat", "codex", "om_progress"); err != nil {
		t.Fatalf("journal: %v", err)
	}
	if got, _ := store.Get(mine.ID); len(got.Interim) != 1 {
		t.Fatalf("running task's journal = %v; want the interim message", got.Interim)
	}
	if got, _ := store.Get(nightly.ID); len(got.Interim) != 0 {
		t.Fatalf("journalled onto the idle lineage: %v", got.Interim)
	}

	// A session reset ends every lineage: they all ran through it.
	if held := store.Holding("chat", "codex"); len(held) != 2 {
		t.Fatalf("holding = %d; want both lineages", len(held))
	}
}

// The listing shows ids to the user, and they are decimal counters: compared
// as text, #10 sorts before #2 as soon as a chat gets past its ninth task.
func TestListingOrdersIdsNumerically(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	frozen := time.Now()
	store.now = func() time.Time { return frozen }
	for i := 0; i < 12; i++ {
		if _, err := store.Create(Task{Channel: "chat", Member: "codex", Goal: "work"}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	var listed []string
	for _, tracked := range store.List("chat") {
		listed = append(listed, tracked.ID)
	}
	// Newest first, and "newest" among equal timestamps means the higher id.
	want := []string{"12", "11", "10", "9", "8", "7", "6", "5", "4", "3", "2", "1"}
	if !slices.Equal(listed, want) {
		t.Fatalf("List order = %v; want %v", listed, want)
	}
}

// A chat task nobody has spoken to for a day is closed; one with a live
// attempt, one spoken to recently, and one a schedule or delegation
// opened are left alone.
func TestCloseIdleEndsQuietChatTasks(t *testing.T) {
	s, clock := newStore(t)
	now := *clock
	*clock = now.Add(-30 * time.Hour)
	old, _ := s.Create(Task{Goal: "old chat", Channel: "c1", Member: "codex"})
	busy, _ := s.Create(Task{Goal: "busy chat", Channel: "c2", Member: "codex"})
	sched, _ := s.Create(Task{Goal: "cron", Channel: "c3", Member: "codex", Origin: "schedule"})
	for _, id := range []string{old.ID, busy.ID, sched.ID} {
		if _, err := s.Begin(id, "codex", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	*clock = now.Add(-time.Hour)
	fresh, _ := s.Create(Task{Goal: "fresh chat", Channel: "c4", Member: "codex"})
	if _, err := s.Begin(fresh.ID, "codex", "", ""); err != nil {
		t.Fatal(err)
	}
	*clock = now
	closed := s.CloseIdle(24*time.Hour, func(id string) bool { return id == busy.ID })
	if len(closed) != 1 || closed[0].ID != old.ID {
		t.Fatalf("closed = %+v", closed)
	}
	for _, id := range []string{busy.ID, sched.ID, fresh.ID} {
		if got, _ := s.Get(id); got.State != StateRunning {
			t.Fatalf("task %s = %s, want running", id, got.State)
		}
	}
}
