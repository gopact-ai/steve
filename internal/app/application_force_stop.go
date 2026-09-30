package app

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"sort"
	"time"
)

func (s *applicationStops) stopBatch(pending []attempt.Record) []attempt.Record {
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	var forced, ordinary []attempt.Record
	for _, r := range pending {
		switch {
		case r.ForceStop == nil || r.ForceStop.Level == "confirmed":
			ordinary = append(ordinary, r)
		case r.ForceStop.Level == "kill":
			forced = append(forced, r)
		}
	}
	take := func(records []attempt.Record, after *string, limit int) []attempt.Record {
		if len(records) == 0 {
			return nil
		}
		start := sort.Search(len(records), func(i int) bool { return records[i].ID > *after })
		if start == len(records) {
			start = 0
		}
		var out []attempt.Record
		for i := range min(len(records), limit) {
			r := records[(start+i)%len(records)]
			out = append(out, r)
			*after = r.ID
		}
		return out
	}
	selected := take(forced, &s.forceAfter, 2)
	return append(selected, take(ordinary, &s.after, 4-len(selected))...)
}

type sessionFailure interface{ SessionErrorCode() string }
type applicationOpenKiller interface {
	KillNodeOpen(context.Context, harness.Placement, string) (nodewire.SessionState, error)
}

func (s *applicationStops) forceStop(parent context.Context, r attempt.Record) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	ctx = execution.WithProbeKey(ctx, execution.Key{TaskID: r.TaskID, InstanceID: r.TurnID, AttemptID: r.ID})
	state, err := s.killOnNode(ctx, r)
	if err == nil {
		stopped, confirmErr := s.attempts.ConfirmForceStopped(parent, r.ID, r.ForceStop.Revision, attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: state})
		if confirmErr != nil {
			return confirmErr
		}
		return s.projectStopped(parent, stopped)
	}
	if parent.Err() != nil {
		return err
	}
	var coded sessionFailure
	answered := errors.As(err, &coded)
	code := ""
	if answered {
		code = coded.SessionErrorCode()
	}
	_, recordErr := s.attempts.RecordForceStopResult(parent, r.ID, r.ForceStop.Revision, answered, code)
	if errors.Is(recordErr, attempt.ErrForceStopChanged) {
		return nil
	}
	return recordErr
}

func (s *applicationStops) killOnNode(ctx context.Context, r attempt.Record) (nodewire.SessionState, error) {
	tracked, ok := s.tasks.Get(r.TaskID)
	if !ok {
		return nodewire.SessionState{}, forceUnavailable{"stop_unproven"}
	}
	if attempt.PendingSessionOpen(r) {
		killer, ok := s.sessions.(applicationOpenKiller)
		if !ok {
			return nodewire.SessionState{}, forceUnavailable{"stop_unsupported"}
		}
		return killer.KillNodeOpen(ctx, harness.Placement{Node: r.Node, Harness: r.Harness}, r.Workspace.Path)
	}
	runner, err := s.sessions.AttachRetainedSession(ctx, harness.Placement{Node: r.Node, Harness: r.Harness}, r.Session, r.Workspace.Path)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	inspector, ok := runner.(harness.RetainedSessionInspector)
	if !ok {
		return nodewire.SessionState{}, forceUnavailable{"stop_unproven"}
	}
	state, err := inspector.InspectRetained(ctx)
	if err != nil {
		return state, err
	}
	if state.ID != r.Session || state.Harness != r.Harness || state.Binding != sessionBinding(r, tracked) || state.Command != nil && state.Command.ID != attempt.InputCommandID(r) {
		return state, forceUnavailable{"stop_unproven"}
	}
	killer, ok := runner.(harness.RetainedKiller)
	if !ok {
		return state, forceUnavailable{"stop_unsupported"}
	}
	return killer.KillRetained(ctx)
}

type forceUnavailable struct{ code string }

func (e forceUnavailable) Error() string            { return e.code }
func (e forceUnavailable) SessionErrorCode() string { return e.code }
