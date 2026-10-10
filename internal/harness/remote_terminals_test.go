package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

type terminalAdmissionTransport struct {
	requests []nodewire.SessionRequest
	err      error
}

type terminalAdmissionFollowTransport struct {
	requests []nodewire.SessionRequest
}

func (tr *terminalAdmissionFollowTransport) NodeSession(_ context.Context, _ string, request nodewire.SessionRequest) (nodewire.SessionState, error) {
	tr.requests = append(tr.requests, request)
	if request.Action == nodewire.SessionActionTerminalAdmit {
		return nodewire.SessionState{}, terminalAdmissionBusy{}
	}
	if request.Action != nodewire.SessionActionPoll || request.WaitMS > 1000 {
		return nodewire.SessionState{}, errors.New("known unconsumed gate was parked beyond its inert lifetime")
	}
	return nodewire.SessionState{Sequence: 2, Command: &nodewire.SessionCommand{ID: request.CommandID, State: nodewire.SessionCommandCompleted, Output: "done"}}, nil
}

type terminalAdmissionBusy struct{}

func (terminalAdmissionBusy) Error() string            { return "original admission owner contended" }
func (terminalAdmissionBusy) SessionErrorCode() string { return "busy" }

func (tr *terminalAdmissionTransport) NodeSession(_ context.Context, _ string, request nodewire.SessionRequest) (nodewire.SessionState, error) {
	tr.requests = append(tr.requests, request)
	return nodewire.SessionState{}, tr.err
}

func terminalProjection() (nodewire.SessionRequest, nodewire.SessionState) {
	binding := nodewire.SessionBinding{NodeID: "worker", TaskID: "task", AttemptID: "attempt", ExecutionEpoch: 1}
	req := nodewire.SessionRequest{Action: nodewire.SessionActionAttach, ID: "original-session", Binding: binding, CommandID: "input", Text: "never replay", InputSequence: 0}
	start := nodewire.TerminalStart{ID: "nt_" + strings.Repeat("a", 64), CommandID: "input", InputSequence: 7, Generation: 2}
	return req, nodewire.SessionState{
		Binding: binding, TerminalAdmission: true,
		Command:               &nodewire.SessionCommand{ID: req.CommandID, State: nodewire.SessionCommandRunning, InputSequence: 7},
		PendingTerminalStarts: []nodewire.TerminalStart{start},
	}
}

func TestManagedTerminalAdmissionUsesAttestedOriginalInputNotReplay(t *testing.T) {
	req, state := terminalProjection()
	tr := &terminalAdmissionTransport{}
	s := &managedSession{transport: tr, at: Placement{Node: "worker"}}
	if err := s.admitReadyTerminals(t.Context(), req, state); err != nil {
		t.Fatal(err)
	}
	if len(tr.requests) != 1 {
		t.Fatal("ready original terminal was not admitted")
	}
	got := tr.requests[0]
	if got.Action != nodewire.SessionActionTerminalAdmit || got.InputSequence != state.Command.InputSequence || got.CommandID != req.CommandID || got.Binding != req.Binding || got.TerminalStart == nil || *got.TerminalStart != state.PendingTerminalStarts[0] || got.Text != "" {
		t.Fatalf("fresh admission differs from the original attested input: %+v", got)
	}
}

func TestManagedTerminalAdmissionRejectsLegacyAndHistoricalProjection(t *testing.T) {
	for _, kind := range []string{"legacy", "binding", "command", "input", "duplicate", "generation", "bounded"} {
		t.Run(kind, func(t *testing.T) {
			req, state := terminalProjection()
			switch kind {
			case "legacy":
				state.TerminalAdmission = false
			case "binding":
				state.Binding.AttemptID = "other"
			case "command":
				state.PendingTerminalStarts[0].CommandID = "old-input"
			case "input":
				state.PendingTerminalStarts[0].InputSequence = 6
			case "duplicate":
				state.PendingTerminalStarts = append(state.PendingTerminalStarts, state.PendingTerminalStarts[0])
			case "generation":
				state.PendingTerminalStarts[0].Generation = 0
			case "bounded":
				state.PendingTerminalStarts = make([]nodewire.TerminalStart, 33)
			}
			tr := &terminalAdmissionTransport{}
			s := &managedSession{transport: tr, at: Placement{Node: "worker"}}
			if err := s.admitReadyTerminals(t.Context(), req, state); err == nil {
				t.Fatal("invalid projection became an execution request")
			}
			if len(tr.requests) != 0 {
				t.Fatal("partially validated projection started a payload before later refusal")
			}
		})
	}
}

func TestManagedTerminalAdmissionDoesNotRetryUnknownReply(t *testing.T) {
	req, state := terminalProjection()
	tr := &terminalAdmissionTransport{err: errors.New("reply lost")}
	s := &managedSession{transport: tr, at: Placement{Node: "worker"}}
	if err := s.admitReadyTerminals(t.Context(), req, state); err == nil || len(tr.requests) != 1 {
		t.Fatal("unknown response was ignored or blindly retried")
	}
}

func TestManagedTerminalAdmissionKeepsObservingKnownUnconsumedContention(t *testing.T) {
	req, state := terminalProjection()
	tr := &terminalAdmissionTransport{err: terminalAdmissionBusy{}}
	s := &managedSession{transport: tr, at: Placement{Node: "worker"}}
	if err := s.admitReadyTerminals(t.Context(), req, state); err != nil || len(tr.requests) != 1 {
		t.Fatal("known unconsumed contention stopped observation or retried without a new poll")
	}
}

func TestManagedTerminalAdmissionContentionUsesShortFreshPoll(t *testing.T) {
	req, state := terminalProjection()
	tr := &terminalAdmissionFollowTransport{}
	s := &managedSession{transport: tr, at: Placement{Node: "worker"}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	output, _, err := s.follow(ctx, req, state, nil, nil, nil)
	if err != nil || output != "done" || len(tr.requests) != 2 || tr.requests[0].Action != nodewire.SessionActionTerminalAdmit {
		t.Fatalf("known unconsumed contention did not preserve short fresh observation: %q %v %+v", output, err, tr.requests)
	}
}
