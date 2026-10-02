package app

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func (s *applicationStops) stopBatch(pending []attempt.Record) []attempt.Record {
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	var forced, ordinary []attempt.Record
	for _, r := range pending {
		switch {
		case r.Abandoned != nil || r.ForceStop == nil || r.ForceStop.Level == "confirmed":
			ordinary = append(ordinary, r)
		case r.ForceStop.Level == "kill" || r.ForceStop.Level == "restart" || r.ForceStop.Level == "await":
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
type applicationSessionKiller interface {
	KillRetainedSession(context.Context, harness.Placement, string, string) (nodewire.SessionState, error)
}

type applicationOpenKiller interface {
	KillNodeOpen(context.Context, harness.Placement, string) (nodewire.SessionState, error)
}

func (s *applicationStops) forceStop(parent context.Context, r attempt.Record) error {
	if r.ForceStop.Level != "kill" {
		return s.restartForceStop(parent, r)
	}
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
	var undispatched *nodewire.SessionNotDispatched
	answered := !errors.As(err, &undispatched) && errors.As(err, &coded)
	code := ""
	if answered {
		code = coded.SessionErrorCode()
	}
	next, recordErr := s.attempts.RecordForceStopResult(parent, r.ID, r.ForceStop.Revision, answered, code)
	if errors.Is(recordErr, attempt.ErrForceStopChanged) {
		return nil
	}
	if recordErr == nil && next.ForceStop.Level == "restart" {
		return s.restartForceStop(parent, next)
	}
	return recordErr
}

func (s *applicationStops) killOnNode(ctx context.Context, r attempt.Record) (nodewire.SessionState, error) {
	if _, ok := s.tasks.Get(r.TaskID); !ok {
		return nodewire.SessionState{}, forceUnavailable{"stop_unproven"}
	}
	if attempt.PendingSessionOpen(r) {
		killer, ok := s.sessions.(applicationOpenKiller)
		if !ok {
			return nodewire.SessionState{}, forceUnavailable{"stop_unsupported"}
		}
		return killer.KillNodeOpen(ctx, harness.Placement{Node: r.Node, Harness: r.Harness}, r.Workspace.Path)
	}
	killer, ok := s.sessions.(applicationSessionKiller)
	if !ok {
		return nodewire.SessionState{}, forceUnavailable{"stop_unsupported"}
	}
	return killer.KillRetainedSession(ctx, harness.Placement{Node: r.Node, Harness: r.Harness}, r.Session, r.Workspace.Path)
}

type forceUnavailable struct{ code string }

func (e forceUnavailable) Error() string            { return e.code }
func (e forceUnavailable) SessionErrorCode() string { return e.code }
