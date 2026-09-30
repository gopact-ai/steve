package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestKillRetainedBypassesStopDoneCache(t *testing.T) {
	done := make(chan struct{})
	close(done)
	s := &managedSession{id: "ns_original", at: Placement{Node: "worker"}, base: NodeSessionContext{Binding: nodewire.SessionBinding{AttemptID: "old"}}, stopDone: done, stopErr: errors.New("earlier abort failed")}
	calls := 0
	s.transport = stopTransportFunc(func(ctx context.Context, _ string, r nodewire.SessionRequest) (nodewire.SessionState, error) {
		calls++
		if r.Action != "kill" {
			t.Errorf("action = %s", r.Action)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("no kill deadline")
		}
		return nodewire.SessionState{ID: r.ID, Binding: r.Binding, ProcessStopped: true}, nil
	})
	killer, ok := any(s).(interface {
		KillRetained(context.Context) (nodewire.SessionState, error)
	})
	if !ok {
		t.Fatal("retained session has no kill")
	}
	for range 2 {
		if _, err := killer.KillRetained(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("kill reused earlier result: %d", calls)
	}
}
func TestKillRetainedRequiresOriginalProcessReceipt(t *testing.T) {
	for _, mode := range []string{"binding", "not-stopped"} {
		t.Run(mode, func(t *testing.T) {
			s := &managedSession{id: "ns_original", base: NodeSessionContext{Binding: nodewire.SessionBinding{AttemptID: "old"}}}
			s.transport = stopTransportFunc(func(_ context.Context, _ string, r nodewire.SessionRequest) (nodewire.SessionState, error) {
				st := nodewire.SessionState{ID: r.ID, Binding: r.Binding, ProcessStopped: true}
				if mode == "binding" {
					st.Binding.AttemptID = "new"
				} else {
					st.ProcessStopped = false
				}
				return st, nil
			})
			killer, ok := any(s).(interface {
				KillRetained(context.Context) (nodewire.SessionState, error)
			})
			if !ok {
				t.Fatal("retained session has no kill")
			}
			if _, err := killer.KillRetained(t.Context()); err == nil {
				t.Fatal("kill accepted an unproven receipt")
			}
		})
	}
}
