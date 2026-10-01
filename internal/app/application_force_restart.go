package app

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func (s *applicationStops) restartForceStop(ctx context.Context, r attempt.Record) error {
	if r.ForceStop.Level == "await" {
		return s.awaitForceStop(ctx, r, false)
	}
	if s.restarts == nil {
		return s.exhaustForceStop(ctx, r, "restart_unavailable")
	}
	if r.ForceStop.RestartID == "" {
		return s.beginForceRestart(ctx, r)
	}
	return s.pollForceRestart(ctx, r)
}

func (s *applicationStops) beginForceRestart(ctx context.Context, r attempt.Record) error {
	target, err := s.restarts.Find(ctx, r.Node, r.ForceStop.By)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return s.exhaustForceStop(ctx, r, restartFailureReason(err, "restart_no_holder"))
	}
	op, fresh, err := s.attempts.BeginForceRestart(ctx, r.ID, r.ForceStop.Revision, target.ClusterID, target.Holder)
	if errors.Is(err, attempt.ErrForceStopChanged) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	// The operation is already durable. Regardless of the response, every later
	// pass only observes it; replaying a dispatch could restart a machine twice.
	return s.restarts.Start(ctx, op)
}

func (s *applicationStops) pollForceRestart(ctx context.Context, r attempt.Record) error {
	op, found, err := s.attempts.ForceRestart(ctx, r.Node)
	if err != nil {
		return err
	}
	if !found || op.ID != r.ForceStop.RestartID || op.Holder != r.ForceStop.RestartHolder {
		return s.awaitForceStop(ctx, r, true)
	}
	if s.now().Sub(r.ForceStop.RestartRequestedAt) >= 7*time.Minute {
		if err := s.attempts.FinishForceRestart(ctx, op, "timeout"); err != nil {
			return err
		}
		return s.exhaustForceStop(ctx, r, "restart_timeout")
	}
	if !op.FinishedAt.IsZero() {
		if op.Outcome == "connected" {
			return s.moveForceAwait(ctx, r)
		}
		return s.awaitForceStop(ctx, r, true)
	}
	if op.PlanID == "" {
		return s.awaitForceStop(ctx, r, true)
	}
	status, err := s.restarts.Status(ctx, op)
	if err != nil {
		var refused cluster.MemberRestartError
		if errors.As(err, &refused) {
			return s.exhaustForceStop(ctx, r, refused.Reason)
		}
		if ctx.Err() != nil {
			return err
		}
		return s.awaitForceStop(ctx, r, true)
	}
	switch status.State {
	case "running", "claiming":
		return nil
	case "connected":
		if err := s.attempts.FinishForceRestart(ctx, op, "connected"); err != nil {
			return err
		}
		return s.moveForceAwait(ctx, r)
	case "failed":
		if err := s.attempts.FinishForceRestart(ctx, op, "failed"); err != nil {
			return err
		}
		return s.exhaustForceStop(ctx, r, "restart_failed")
	default:
		return s.awaitForceStop(ctx, r, true)
	}
}

// A missing restart status is never permission to dispatch SSH again. A fresh
// node reply may permit L3; silence exhausts the operation conservatively.
func (s *applicationStops) awaitForceStop(parent context.Context, r attempt.Record, lost bool) error {
	if !lost && s.now().Sub(r.ForceStop.LevelSince) >= 2*time.Minute {
		return s.exhaustForceStop(parent, r, "await_timeout")
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	ctx = execution.WithProbeKey(ctx, execution.Key{TaskID: r.TaskID, InstanceID: r.TurnID, AttemptID: r.ID})
	state, err := s.killOnNode(ctx, r)
	if err == nil {
		stopped, err := s.attempts.ConfirmForceStopped(parent, r.ID, r.ForceStop.Revision, attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: state})
		if errors.Is(err, attempt.ErrForceStopChanged) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.ForceStop.RestartID != "" {
			if op, found, readErr := s.attempts.ForceRestart(parent, r.Node); readErr != nil {
				return readErr
			} else if found && op.ID == r.ForceStop.RestartID {
				if err := s.attempts.FinishForceRestart(parent, op, "connected"); err != nil {
					return err
				}
			}
		}
		return s.projectStopped(parent, stopped)
	}
	if parent.Err() != nil {
		return err
	}
	var coded sessionFailure
	var undispatched *nodewire.SessionNotDispatched
	answered := !errors.As(err, &undispatched) && errors.As(err, &coded)
	if answered {
		switch coded.SessionErrorCode() {
		case "stop_running", "stop_unproven", "stop_unsupported":
			return s.exhaustForceStop(parent, r, coded.SessionErrorCode())
		case "invalid":
			return s.exhaustForceStop(parent, r, "upgrade_required")
		case "forbidden", "conflict", "absent":
			return s.exhaustForceStop(parent, r, "rejected")
		}
	}
	if lost {
		if op, found, readErr := s.attempts.ForceRestart(parent, r.Node); readErr != nil {
			return readErr
		} else if found && op.ID == r.ForceStop.RestartID {
			outcome := "lost"
			if answered {
				outcome = "connected"
			}
			if err := s.attempts.FinishForceRestart(parent, op, outcome); err != nil {
				return err
			}
		}
		if answered {
			return s.moveForceAwait(parent, r)
		}
		return s.exhaustForceStop(parent, r, "restart_status_lost")
	}
	return nil
}

func (s *applicationStops) moveForceAwait(ctx context.Context, r attempt.Record) error {
	_, err := s.attempts.AdvanceForceStop(ctx, r.ID, r.ForceStop.Revision, "restart", "await", "")
	if errors.Is(err, attempt.ErrForceStopChanged) {
		return nil
	}
	return err
}
func (s *applicationStops) exhaustForceStop(ctx context.Context, r attempt.Record, reason string) error {
	_, err := s.attempts.AdvanceForceStop(ctx, r.ID, r.ForceStop.Revision, r.ForceStop.Level, "exhausted", reason)
	if errors.Is(err, attempt.ErrForceStopChanged) {
		return nil
	}
	return err
}
func restartFailureReason(err error, otherwise string) string {
	var refused cluster.MemberRestartError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return otherwise
}
