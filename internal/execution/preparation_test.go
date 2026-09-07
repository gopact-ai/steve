package execution

import (
	"context"
	"errors"
	"testing"
)

func TestUnknownNodePreparationDetachesOnlyAfterWholeLifetimeEnds(t *testing.T) {
	for _, matching := range []bool{true, false} {
		t.Run(map[bool]string{true: "original-attempt", false: "wrong-attempt"}[matching], func(t *testing.T) {
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := New(lifetime, nil)
			s, err := r.Begin(t.Context(), Key{AttemptID: "admitted-attempt"})
			if err != nil {
				t.Fatal(err)
			}
			id := "admitted-attempt"
			if !matching {
				id = "another-attempt"
			}
			unknown := errors.New("node open response was lost")
			s.Finish(&NodePreparationObserverDetached{AttemptID: id, NodeID: "worker", OpenCommandID: "original/open", Cause: unknown})
			if err := (WaitSet{s}).Wait(t.Context()); !errors.Is(err, unknown) {
				t.Fatalf("explicit stop invented preparation settlement: %v", err)
			}
			if err := r.Shutdown(t.Context()); !errors.Is(err, unknown) {
				t.Fatalf("live service ignored preparation uncertainty: %v", err)
			}
			cancel()
			err = r.Shutdown(t.Context())
			if matching != (err == nil) {
				t.Fatalf("ended service classified another node owner: %v", err)
			}
			if err := (WaitSet{s}).Wait(t.Context()); !errors.Is(err, unknown) {
				t.Fatalf("global join erased explicit stop uncertainty: %v", err)
			}
		})
	}
}
