package task

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTreeSpendIsAtomicAndIndependentOfFinishOrder(t *testing.T) {
	for _, childFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "grandchild-first", true: "child-first"}[childFirst], func(t *testing.T) {
			s, clock := newStore(t)
			r, _ := s.Create(Task{Member: "root"})
			c, _ := s.Spawn(r.ID, Task{Member: "child"})
			g, _ := s.Spawn(c.ID, Task{Member: "grandchild"})
			for _, x := range []Task{c, g} {
				if _, err := s.Begin(x.ID, x.Member, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			root, _ := s.Get(r.ID)
			if root.Budget.Turns != 2 {
				t.Fatalf("running work was not charged atomically: %+v", root.Budget)
			}
			*clock = clock.Add(10 * time.Second)
			order := []Task{g, c}
			if childFirst {
				order = []Task{c, g}
			}
			for _, x := range order {
				tokens := Tokens{Total: 100}
				if x.ID == g.ID {
					tokens.Total = 200
				}
				if _, err := s.FinishAs(x.ID, OutcomeOK, tokens, 2, "test-model"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.FinishAs(x.ID, OutcomeOK, tokens, 2, "test-model"); err == nil {
					t.Fatal("duplicate finish charged the tree again")
				}
			}
			for _, id := range []string{r.ID, c.ID} {
				x, _ := s.Get(id)
				if x.Budget.Turns != 2 || x.Budget.Tokens.Total != 300 || x.Budget.Elapsed != 20*time.Second || x.Budget.ToolCalls != 4 {
					t.Fatalf("wrong subtree spend for %s: %+v", id, x.Budget)
				}
			}
			reopened, err := openWith(s.doc)
			if err != nil {
				t.Fatal(err)
			}
			root, _ = reopened.Get(r.ID)
			if root.Budget.Tokens.Total != 300 {
				t.Fatalf("tree spend was not persisted: %+v", root.Budget)
			}
		})
	}
}

func TestSiblingBeginsCannotOverspendTheAncestor(t *testing.T) {
	s, _ := newStore(t)
	r, _ := s.Create(Task{Member: "root", Budget: Budget{MaxTurns: 2}})
	var children []Task
	for range 12 {
		child, err := s.Spawn(r.ID, Task{Member: "child"})
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, child := range children {
		wg.Go(func() {
			if _, err := s.Begin(child.ID, child.Member, "", ""); err == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	root, _ := s.Get(r.ID)
	if successes.Load() != 2 || root.Budget.Turns != 2 {
		t.Fatalf("ancestor budget overspent: successes=%d budget=%+v", successes.Load(), root.Budget)
	}
	if _, err := s.Begin(r.ID, "root", "", ""); err == nil {
		t.Fatal("parent's own turn bypassed child spend")
	}
}

func TestBeginRefusesAnAlreadyOpenAttempt(t *testing.T) {
	s, _ := newStore(t)
	r, _ := s.Create(Task{Member: "root"})
	c, _ := s.Spawn(r.ID, Task{Member: "child"})
	if _, err := s.Begin(c.ID, "child", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(c.ID, "child", "", ""); err == nil {
		t.Fatal("opened a second attempt before the first finished")
	}
	root, _ := s.Get(r.ID)
	child, _ := s.Get(c.ID)
	if root.Budget.Turns != 1 || len(child.Attempts) != 1 {
		t.Fatalf("rejected begin changed tree: root=%+v child=%+v", root.Budget, child)
	}
}

func TestTreeBudgetWriteFailureDoesNotPartiallyCharge(t *testing.T) {
	s, clock := newStore(t)
	r, _ := s.Create(Task{Member: "root"})
	c, _ := s.Spawn(r.ID, Task{Member: "child"})
	doc := &metaDocument{Doc: s.doc, fail: true}
	s.doc = doc
	if _, err := s.Begin(c.ID, "child", "", ""); err == nil {
		t.Fatal("failed begin write was acknowledged")
	}
	root, _ := s.Get(r.ID)
	child, _ := s.Get(c.ID)
	if root.Budget.Turns != 0 || child.Budget.Turns != 0 || len(child.Attempts) != 0 {
		t.Fatal("failed begin partially charged tree")
	}
	doc.fail = false
	if _, err := s.Begin(c.ID, "child", "", ""); err != nil {
		t.Fatal(err)
	}
	root, _ = s.Get(r.ID)
	child, _ = s.Get(c.ID)
	*clock = clock.Add(time.Second)
	doc.fail = true
	if _, err := s.Finish(c.ID, OutcomeOK, Tokens{Total: 9}, 1); err == nil {
		t.Fatal("failed finish write was acknowledged")
	}
	stillRoot, _ := s.Get(r.ID)
	stillChild, _ := s.Get(c.ID)
	if !reflect.DeepEqual(root, stillRoot) || !reflect.DeepEqual(child, stillChild) {
		t.Fatal("failed finish partially charged tree")
	}
	doc.fail = false
	if _, err := s.Finish(c.ID, OutcomeOK, Tokens{Total: 9}, 1); err != nil {
		t.Fatal(err)
	}
	root, _ = s.Get(r.ID)
	if root.Budget.Turns != 1 || root.Budget.Tokens.Total != 9 || root.Budget.Elapsed != time.Second {
		t.Fatalf("retry did not charge once: %+v", root.Budget)
	}
}
