package task

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

// A write to one task costs the same however much history sits beside it.
func TestWriteWorkDoesNotGrowWithHistory(t *testing.T) {
	allocs := func(history int) (meta, task float64) {
		s, _ := newStore(t)
		for i := range history {
			mustCreate(t, s, fmt.Sprint("past ", i), "past")
		}
		target := mustCreate(t, s, "live", "live")
		i := 0
		meta = testing.AllocsPerRun(20, func() {
			i++
			title := fmt.Sprint("title ", i)
			if _, err := s.SetMeta(target.ID, MetaPatch{Title: &title}); err != nil {
				t.Fatal(err)
			}
		})
		task = testing.AllocsPerRun(20, func() {
			i++
			if err := s.AddInterimForTask(target.ID, fmt.Sprint("message ", i)); err != nil {
				t.Fatal(err)
			}
		})
		return meta, task
	}
	smallMeta, smallTask := allocs(10)
	largeMeta, largeTask := allocs(1000)
	if largeMeta-smallMeta > 100 || largeTask-smallTask > 100 {
		t.Fatalf("allocations per write grew with history: meta %.0f -> %.0f, task %.0f -> %.0f",
			smallMeta, largeMeta, smallTask, largeTask)
	}
}

// lineageStore is a root with one running child, plus an unrelated task.
func lineageStore(t *testing.T) (s *Store, clock *time.Time, root, child, other Task) {
	t.Helper()
	s, clock = newStore(t)
	root = mustCreate(t, s, "root", "chat")
	other = mustCreate(t, s, "other", "elsewhere")
	if _, err := s.Advance(root.ID, StateRunning); err != nil {
		t.Fatal(err)
	}
	child, err := s.Spawn(root.ID, Task{Goal: "child", Channel: "chat", Member: "helper"})
	if err != nil {
		t.Fatal(err)
	}
	return s, clock, root, child, other
}

func snapshotTasks(s *Store) map[string]Task {
	out := map[string]Task{}
	for _, stored := range s.List("") {
		out[stored.ID] = stored
	}
	return out
}

// A refused durable write leaves every task it touched as it was, in memory
// and in the ledger, including ancestors a lineage write charged.
func TestRefusedWriteLeavesEveryTouchedTask(t *testing.T) {
	s, clock, root, child, _ := lineageStore(t)
	if _, err := s.Begin(child.ID, "helper", "node", ""); err != nil {
		t.Fatal(err)
	}
	before := snapshotTasks(s)
	gate := gateWrites(t, s)
	gate.fail = true
	*clock = clock.Add(time.Minute)
	if _, err := s.Finish(child.ID, OutcomeOK, Tokens{Total: 5}, 2); err == nil {
		t.Fatal("finish landed through a refused write")
	}
	if _, err := s.SetAside(root.ID, StatePaused); err == nil {
		t.Fatal("stop landed through a refused write")
	}
	if got := snapshotTasks(s); !reflect.DeepEqual(got, before) {
		t.Fatalf("refused writes changed memory:\n got %+v\nwant %+v", got, before)
	}
	gate.fail = false
	reopened, err := OpenLedger(s.book)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshotTasks(reopened); !reflect.DeepEqual(got, before) {
		t.Fatalf("refused writes reached the ledger:\n got %+v\nwant %+v", got, before)
	}
	if _, err := s.Finish(child.ID, OutcomeOK, Tokens{Total: 5}, 2); err != nil {
		t.Fatalf("finish after the refusal: %v", err)
	}
}

// What a write returns, and what a caller did with an earlier read, is the
// caller's copy: changing it cannot rewrite the store.
func TestWriteResultsDoNotAliasTheStore(t *testing.T) {
	s, _, root, child, _ := lineageStore(t)
	began, err := s.Begin(child.ID, "helper", "node", "")
	if err != nil {
		t.Fatal(err)
	}
	read, _ := s.Get(root.ID)
	before := snapshotTasks(s)
	began.Attempts[0].Member = "someone else"
	began.Budget.Turns = 99
	read.Budget.Turns = 99
	if got := snapshotTasks(s); !reflect.DeepEqual(got, before) {
		t.Fatal("a returned task aliases the store")
	}
	finished, err := s.Finish(child.ID, OutcomeOK, Tokens{Total: 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Attempts[0].Member != "helper" || finished.Budget.Turns != 1 {
		t.Fatalf("finish built on a caller's copy: %+v", finished)
	}
}

// Every successful write persists exactly the tasks and metadata memory
// serves, including deletions, and announces only the tasks it changed.
func TestWritesPersistWhatMemoryServes(t *testing.T) {
	s, clock, root, child, other := lineageStore(t)
	observed := make(chan string, 64)
	s.SetObserver(func(id string) { observed <- id })
	heard := func() []string {
		var ids []string
		for {
			select {
			case id := <-observed:
				ids = append(ids, id)
			case <-time.After(100 * time.Millisecond):
				sort.Strings(ids)
				return ids
			}
		}
	}
	title := "titled"
	if _, err := s.SetMeta(other.ID, MetaPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if got := heard(); !reflect.DeepEqual(got, []string{other.ID}) {
		t.Fatalf("metadata write announced %v", got)
	}
	*clock = clock.Add(time.Minute)
	if _, err := s.Begin(child.ID, "helper", "node", ""); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	if _, err := s.Finish(child.ID, OutcomeOK, Tokens{Total: 3}, 1); err != nil {
		t.Fatal(err)
	}
	// Begin and Finish each charge the child and its root, nothing else.
	want := []string{root.ID, root.ID, child.ID, child.ID}
	sort.Strings(want)
	if got := heard(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage writes announced %v, want %v", got, want)
	}
	*clock = clock.Add(time.Minute)
	if err := s.AddInterimForTask(root.ID, "m1"); err != nil {
		t.Fatal(err)
	}
	if got := heard(); !reflect.DeepEqual(got, []string{root.ID}) {
		t.Fatalf("interim write announced %v", got)
	}
	if _, err := s.Advance(other.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	gone := mustCreate(t, s, "gone", "gone")
	if _, err := s.SetMeta(gone.ID, MetaPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteChannel("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAside(root.ID, StatePaused); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(s.book)
	if err != nil {
		t.Fatal(err)
	}
	served := snapshotTasks(s)
	if _, ok := served[gone.ID]; ok {
		t.Fatal("deleted task is still served")
	}
	if got := snapshotTasks(reopened); !reflect.DeepEqual(got, served) {
		t.Fatalf("reopened tasks differ:\n got %+v\nwant %+v", got, served)
	}
	for _, id := range []string{root.ID, child.ID, other.ID, gone.ID} {
		if got, want := reopened.MetaOf(id), s.MetaOf(id); !reflect.DeepEqual(got, want) {
			t.Fatalf("reopened metadata %s = %+v, want %+v", id, got, want)
		}
	}
	assertReadIndexMatchesStartup(t, s)
}

// What a caller passed to Create or Spawn stays the caller's: changing its
// slices and pointers afterwards cannot rewrite the stored task.
func TestCreatedTasksDoNotAliasTheirInput(t *testing.T) {
	s, _, root, _, _ := lineageStore(t)
	input := func() Task {
		return Task{Goal: "input", Channel: "chat", Member: "writer",
			Interim:           []string{"m1"},
			Result:            &Result{Answer: "kept", Refs: []string{"ref-1"}},
			Delivery:          &Delivery{State: DeliveryPending, Key: "k"},
			RecoveryWorkspace: &RecoveryWorkspace{ID: "w"}}
	}
	scribble := func(in Task) {
		in.Interim[0] = "changed"
		in.Result.Answer = "changed"
		in.Result.Refs[0] = "changed"
		in.Delivery.State = DeliveryDelivered
		in.RecoveryWorkspace.ID = "changed"
	}
	created := input()
	made, err := s.Create(created)
	if err != nil {
		t.Fatal(err)
	}
	spawnedInput := input()
	spawned, err := s.Spawn(root.ID, spawnedInput)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTasks(s)
	scribble(created)
	scribble(spawnedInput)
	after := snapshotTasks(s)
	for _, id := range []string{made.ID, spawned.ID} {
		if !reflect.DeepEqual(after[id], before[id]) {
			t.Fatalf("task %s changed with its caller's input:\n got %+v\nwant %+v", id, after[id], before[id])
		}
	}
}
