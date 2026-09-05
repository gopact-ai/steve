package task

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func treeStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetBudget(10, time.Hour)
	return s
}

func TestChildIsFundedFromParentRemainder(t *testing.T) {
	s := treeStore(t)
	root, err := s.Create(Task{Goal: "root", Channel: "c", Member: "a"})
	if err != nil {
		t.Fatal(err)
	}
	// Spend some of the parent first.
	for range 3 {
		if _, err := s.Begin(root.ID, "a", "hub", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Finish(root.ID, OutcomeOK, Tokens{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	child, err := s.Spawn(root.ID, Task{Goal: "child", Member: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent != root.ID {
		t.Fatalf("child parent = %q", child.Parent)
	}
	if child.Budget.MaxTurns != 7 {
		t.Fatalf("child ceiling = %d turns, want the parent's 7 remaining", child.Budget.MaxTurns)
	}
	if child.Budget.MaxElapsed > time.Hour {
		t.Fatalf("child time ceiling %s exceeds the parent's", child.Budget.MaxElapsed)
	}
	// A caller may ration below the remainder, never above it.
	rationed, err := s.Spawn(root.ID, Task{Goal: "small", Member: "c", Budget: Budget{MaxTurns: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if rationed.Budget.MaxTurns != 2 {
		t.Fatalf("rationed child got %d turns, asked for 2", rationed.Budget.MaxTurns)
	}
	greedy, err := s.Spawn(root.ID, Task{Goal: "big", Member: "d", Budget: Budget{MaxTurns: 99}})
	if err != nil {
		t.Fatal(err)
	}
	if greedy.Budget.MaxTurns != 7 {
		t.Fatalf("child asked for 99 and got %d; must be capped at the parent's remainder", greedy.Budget.MaxTurns)
	}
}

// What a child spends is no longer available to the rest of the tree.
func TestChargeReachesTheRoot(t *testing.T) {
	s := treeStore(t)
	root, _ := s.Create(Task{Goal: "root", Channel: "c", Member: "a"})
	child, err := s.Spawn(root.ID, Task{Goal: "child", Member: "b"})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := s.Spawn(child.ID, Task{Goal: "grandchild", Member: "c"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.Begin(grandchild.ID, "c", "node-b", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Finish(grandchild.ID, OutcomeOK, Tokens{Total: 500}, 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Charge(grandchild.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(root.ID)
	if got.Budget.Turns != 2 || got.Budget.Tokens.Total != 1000 || got.Budget.ToolCalls != 6 {
		t.Fatalf("root budget after charge = %+v; the grandchild's spend did not reach it", got.Budget)
	}
	mid, _ := s.Get(child.ID)
	if mid.Budget.Turns != 2 {
		t.Fatalf("middle task budget = %+v", mid.Budget)
	}
	// And the root's remainder is now smaller for the next delegation.
	next, err := s.Spawn(root.ID, Task{Goal: "later", Member: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if next.Budget.MaxTurns != 8 {
		t.Fatalf("after the charge the root had %d turns to give, want 8", next.Budget.MaxTurns)
	}
}

func TestExhaustedParentCannotDelegate(t *testing.T) {
	s := treeStore(t)
	s.SetBudget(1, time.Hour)
	root, _ := s.Create(Task{Goal: "root", Channel: "c", Member: "a"})
	if _, err := s.Begin(root.ID, "a", "hub", ""); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Finish(root.ID, OutcomeOK, Tokens{}, 0)
	if _, err := s.Spawn(root.ID, Task{Goal: "child", Member: "b"}); err == nil {
		t.Fatal("a spent parent delegated anyway")
	} else if !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("error = %v", err)
	}
}

// A→B→A is refused in the structure, not discouraged in a prompt.
func TestCycleIsRefused(t *testing.T) {
	s := treeStore(t)
	root, _ := s.Create(Task{Goal: "root", Channel: "c", Member: "a"})
	child, err := s.Spawn(root.ID, Task{Goal: "child", Member: "b"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Spawn(child.ID, Task{Goal: "back to a", Member: "a"})
	if err == nil {
		t.Fatal("a delegation cycle was accepted")
	}
	if !strings.Contains(err.Error(), "loop") {
		t.Fatalf("error = %v, want it to say this loops", err)
	}
	// A different member under the same chain is fine.
	if _, err := s.Spawn(child.ID, Task{Goal: "on to c", Member: "c"}); err != nil {
		t.Fatalf("a non-cyclic delegation was refused: %v", err)
	}
}

func TestDepthIsBounded(t *testing.T) {
	s := treeStore(t)
	current, _ := s.Create(Task{Goal: "root", Channel: "c", Member: "m0"})
	members := []string{"m1", "m2", "m3", "m4", "m5"}
	var err error
	depth := 1
	for _, m := range members {
		var next Task
		next, err = s.Spawn(current.ID, Task{Goal: "deeper", Member: m})
		if err != nil {
			break
		}
		current = next
		depth++
	}
	if err == nil {
		t.Fatal("delegation nested without limit")
	}
	if depth != MaxDepth {
		t.Fatalf("stopped at depth %d, want %d", depth, MaxDepth)
	}
	if !strings.Contains(err.Error(), "deep") {
		t.Fatalf("error = %v", err)
	}
}

func TestAncestryAndChildren(t *testing.T) {
	s := treeStore(t)
	root, _ := s.Create(Task{Goal: "root goal", Channel: "c", Member: "a"})
	child, _ := s.Spawn(root.ID, Task{Goal: "child goal", Member: "b"})
	grandchild, _ := s.Spawn(child.ID, Task{Goal: "grandchild goal", Member: "c"})

	up := s.Ancestry(grandchild.ID)
	if len(up) != 2 || up[0].ID != child.ID || up[1].ID != root.ID {
		t.Fatalf("ancestry = %v, want [child root]", ids(up))
	}
	down := s.Children(root.ID)
	if len(down) != 1 || down[0].ID != child.ID {
		t.Fatalf("children of root = %v", ids(down))
	}
}

func ids(tasks []Task) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.ID
	}
	return out
}

// A parent without a ceiling — the default since budgets became opt-in —
// delegates freely: its children run as long as it does. Zero minus what
// it spent must never read as "nothing left".
func TestUnlimitedParentDelegatesWithoutACeiling(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "tasks.json")) // the defaults: no ceiling
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.Create(Task{Goal: "root", Channel: "c", Member: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if root.Budget.MaxTurns != 0 || root.Budget.MaxElapsed != 0 {
		t.Fatalf("root budget = %+v, want none", root.Budget)
	}
	if _, err := s.Begin(root.ID, "a", "hub", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(root.ID, OutcomeOK, Tokens{}, 60); err != nil {
		t.Fatal(err)
	}
	child, err := s.Spawn(root.ID, Task{Goal: "child", Member: "b"})
	if err != nil {
		t.Fatalf("an unlimited parent could not delegate: %v", err)
	}
	if child.Budget.MaxTurns != 0 || child.Budget.MaxElapsed != 0 {
		t.Fatalf("child budget = %+v, want none, like the parent's", child.Budget)
	}
	// A caller may still ration a child.
	rationed, err := s.Spawn(root.ID, Task{Goal: "small", Member: "c", Budget: Budget{MaxTurns: 2, MaxElapsed: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	if rationed.Budget.MaxTurns != 2 || rationed.Budget.MaxElapsed != time.Minute {
		t.Fatalf("rationed child budget = %+v", rationed.Budget)
	}
}
