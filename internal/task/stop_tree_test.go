package task

import (
	"fmt"
	"testing"
)

func stopTreeStore(total, descendants int) *Store {
	s := &Store{data: data{Tasks: map[string]*Task{}}}
	s.data.Tasks["root"] = &Task{ID: "root", State: StatePaused, ExecutionEpoch: 5}
	for i := 0; i < total; i++ {
		id := fmt.Sprint(i)
		parent := "other"
		if i < descendants {
			parent = "root"
		}
		s.data.Tasks[id] = &Task{ID: id, Parent: parent, State: StatePaused, ExecutionEpoch: 5, System: i%2 == 0}
	}
	return s
}

func TestStopTreeIncludesHiddenTasksAndOwnsItsSnapshot(t *testing.T) {
	s := stopTreeStore(1000, 100)
	tree, err := s.Tree("root")
	if err != nil || len(tree) != 101 {
		t.Fatalf("tree = %d, %v", len(tree), err)
	}
	for i := range tree {
		tree[i].State = StateRunning
	}
	for _, row := range s.data.Tasks {
		if row.State != StatePaused {
			t.Fatal("snapshot aliases task authority")
		}
	}
}

func BenchmarkStopTreeSnapshot(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			s := stopTreeStore(size, 500)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if tree, err := s.Tree("root"); err != nil || len(tree) != 501 {
					b.Fatal(err, len(tree))
				}
			}
		})
	}
}
