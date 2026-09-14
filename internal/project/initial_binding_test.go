package project

import (
	"errors"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestInitialBindingIsIdempotentAndNeverSwitches(t *testing.T) {
	s := openStore(t)
	if err := s.Declare(t.Context(), []Project{{ID: "a", Home: Home{Path: "/a"}}, {ID: "b", Home: Home{Path: "/b"}}}); err != nil {
		t.Fatal(err)
	}
	first, err := s.BindInitial(t.Context(), "chat", "a", "owner")
	if err != nil || first.Version != 1 {
		t.Fatalf("initial = %+v, %v", first, err)
	}
	restarted := Open(s.l)
	again, err := restarted.BindInitial(t.Context(), "chat", "a", "retry")
	if err != nil || again != first {
		t.Fatalf("retry changed binding: %+v, %v", again, err)
	}
	if _, err := s.BindInitial(t.Context(), "chat", "b", "owner"); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("different project = %v", err)
	}
	got, _, _ := s.Binding(t.Context(), "chat")
	if got != first {
		t.Fatalf("initialization switched binding: %+v", got)
	}
	if _, err := s.BindInitial(t.Context(), "unknown", "missing", "owner"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown = %v", err)
	}
	if _, ok, _ := s.Binding(t.Context(), "unknown"); ok {
		t.Fatal("unknown project left a binding")
	}
}

func TestConcurrentInitialBindingHasOneVersion(t *testing.T) {
	for _, competing := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "different"}[competing], func(t *testing.T) {
			s := openStore(t)
			if err := s.Declare(t.Context(), []Project{{ID: "a", Home: Home{Path: "/a"}}, {ID: "b", Home: Home{Path: "/b"}}}); err != nil {
				t.Fatal(err)
			}
			type result struct {
				binding Binding
				err     error
			}
			results := make(chan result, 20)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for i := range 20 {
				workers.Go(func() {
					<-start
					projectID := "a"
					if competing && i%2 == 1 {
						projectID = "b"
					}
					b, err := s.BindInitial(t.Context(), "chat", projectID, "owner")
					results <- result{b, err}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			stored, ok, err := s.Binding(t.Context(), "chat")
			if err != nil || !ok || stored.Version != 1 {
				t.Fatalf("stored = %+v, %v", stored, err)
			}
			for got := range results {
				if got.err != nil {
					if !competing || !errors.Is(got.err, ledger.ErrConflict) {
						t.Errorf("unexpected failure: %v", got.err)
					}
					continue
				}
				if got.binding != stored {
					t.Errorf("returned a different binding: %+v", got.binding)
				}
			}
		})
	}
}
