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

func TestRecoveryIdleCloseUsesOnlyStoppingAndRetainsTheMachineOutcome(t *testing.T) {
	for _, outcome := range []string{"stopped", "interrupted", "nil outcome", "binding", "command", "context", "idle", "running", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			binding := NodeSessionContext{Binding: nodewire.SessionBinding{NodeID: "worker", AttemptID: "copy-turn"}, CommandID: "copy-command"}
			calls := 0
			manager, err := NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Stop()
			manager.SetNodeSessionBinder(func(ctx context.Context, _ Placement, id, work string) (context.Context, error) {
				if id != "ns_copy" || work != "/copy/work" {
					t.Fatal("close lost its exact identity")
				}
				return WithNodeSession(ctx, binding), nil
			})
			manager.remote = directKillTransport{call: func(_ context.Context, node string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
				calls++
				if req.Action != nodewire.SessionActionClose || node != "worker" || req.Binding != binding.Binding || req.CommandID != binding.CommandID {
					t.Fatal("idle retirement used observation or another binding")
				}
				st := nodewire.SessionState{ID: req.ID, ContextID: "native-copy", Harness: "test", Binding: req.Binding, State: nodewire.SessionClosed, ProcessStopped: true, Command: &nodewire.SessionCommand{ID: req.CommandID}}
				switch outcome {
				case "interrupted":
					st.State = nodewire.SessionInterrupted
				case "idle":
					st.State = nodewire.SessionIdle
				case "running":
					st.State = nodewire.SessionRunning
				case "unknown":
					st.State = "unknown"
				case "nil outcome":
					return nodewire.SessionState{}, nil
				case "binding":
					st.Binding.AttemptID = "newer"
				case "command":
					st.Command.ID = "later"
				case "context":
					st.ProcessStopped = false
				}
				return st, nil
			}}
			got, err := manager.CloseRecoverySession(t.Context(), Placement{Node: "worker", Harness: "test"}, "ns_copy", "/copy/work")
			if (outcome == "stopped" || outcome == "interrupted") && (err != nil || !got.ProcessStopped || got.ContextID != "native-copy" || got.Command == nil) {
				t.Fatalf("idle close discarded proof: %+v %v", got, err)
			}
			if outcome != "stopped" && outcome != "interrupted" && err == nil {
				t.Fatalf("unproved close became success: %+v", got)
			}
			if calls != 1 {
				t.Fatalf("close retried or observed first: %d", calls)
			}
		})
	}
}
