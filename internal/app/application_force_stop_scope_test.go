package app

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type stoppingOnlyTransport struct {
	calls   []nodewire.SessionAction
	state   nodewire.SessionState
	refusal string
	invalid string
}

func (*stoppingOnlyTransport) Transport(string, string) acphost.Transport { return nil }
func (r *stoppingOnlyTransport) NodeSession(_ context.Context, _ string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	r.calls = append(r.calls, req.Action)
	if req.Action != nodewire.SessionActionKill {
		return nodewire.SessionState{}, &node.SessionError{Code: "forbidden", Message: r.refusal}
	}
	if req.ID != r.state.ID || req.Binding != r.state.Binding || req.CommandID != "turn-0" {
		return nodewire.SessionState{}, &node.SessionError{Code: "conflict", Message: "wrong original execution"}
	}
	result := r.state
	result.ProcessStopped = true
	if r.invalid == "binding" {
		result.Binding.AttemptID = "another"
	}
	if r.invalid == "exit" {
		result.ProcessStopped = false
	}
	return result, nil
}
func TestForceStopUsesStoppingAuthorityWithoutObservation(t *testing.T) {
	for _, scope := range []string{"project", "node"} {
		t.Run(scope, func(t *testing.T) {
			s, records, sessions := forceFixture(t, 1)
			r := records[0]
			manager, err := harness.NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Stop()
			transport := &stoppingOnlyTransport{state: sessions.states[r.Session], refusal: "plugin " + scope + " authorization was revoked"}
			manager.SetTransports(transport)
			manager.SetNodeSessionBinder(func(ctx context.Context, _ harness.Placement, _, _ string) (context.Context, error) {
				return harness.WithNodeSession(ctx, harness.NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "test", CoordinatorNodeID: "coordinator", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: transport.state.Binding, CommandID: r.TurnID}), nil
			})
			s.sessions = manager
			if _, err := s.attempts.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			if err := s.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, _ := s.attempts.Get(t.Context(), r.ID)
			if len(transport.calls) != 1 || transport.calls[0] != nodewire.SessionActionKill || got.ForceStop.Level != "confirmed" {
				t.Fatalf("cleanup-authorized kill blocked: calls=%v force=%+v", transport.calls, got.ForceStop)
			}
		})
	}
}

func TestAnsweredBadKillProofResetsTheUnansweredWindow(t *testing.T) {
	for _, invalid := range []string{"binding", "exit"} {
		t.Run(invalid, func(t *testing.T) {
			s, records, sessions := forceFixture(t, 1)
			r := records[0]
			manager, err := harness.NewManager(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Stop()
			transport := &stoppingOnlyTransport{state: sessions.states[r.Session], invalid: invalid}
			manager.SetTransports(transport)
			manager.SetNodeSessionBinder(func(ctx context.Context, _ harness.Placement, _, _ string) (context.Context, error) {
				return harness.WithNodeSession(ctx, harness.NodeSessionContext{Authority: nodewire.SessionAuthority{ClusterID: "test", CoordinatorNodeID: "coordinator", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: transport.state.Binding, CommandID: r.TurnID}), nil
			})
			s.sessions = manager
			if _, err := s.attempts.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.attempts.RecordForceStopResult(t.Context(), r.ID, 1, false, ""); err != nil {
				t.Fatal(err)
			}
			if err := s.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, _ := s.attempts.Get(t.Context(), r.ID)
			if got.ForceStop.Level != "exhausted" || got.ForceStop.Reason != "stop_unproven" || got.ForceStop.UnansweredCount != 0 || !got.ForceStop.UnansweredSince.IsZero() {
				t.Fatalf("answered invalid proof looks like transport silence: %+v", got.ForceStop)
			}
		})
	}
}
