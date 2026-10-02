package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type ForceStop struct {
	RestartID          string    `json:"restart_id,omitempty"`
	RestartHolder      string    `json:"restart_holder,omitempty"`
	RestartRequestedAt time.Time `json:"restart_requested_at,omitempty"`
	Revision           uint64    `json:"revision"`
	RequestedAt        time.Time `json:"requested_at"`
	By                 string    `json:"by"`
	Level              string    `json:"level"`
	LevelSince         time.Time `json:"level_since"`
	UnansweredSince    time.Time `json:"unanswered_since,omitempty"`
	UnansweredCount    uint8     `json:"unanswered_count,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	ExhaustedAt        time.Time `json:"exhausted_at,omitempty"`
}

var ErrForceStopChanged = errors.New("force stop request changed")

// RequestForceStop records an owner request only for an already revoked
// native execution. A newer revision fences every older in-flight result.
func (s *Service) RequestForceStop(ctx context.Context, id, actor string) (Record, error) {
	if actor == "" {
		return Record{}, errors.New("force stop requires an actor")
	}
	return s.writeForceStop(ctx, id, 0, actor, func(r *Record) error {
		if r.Abandoned != nil || !TaskStopOwed(*r) || TaskStopConfirmed(*r) {
			return errors.New("execution does not owe a native stop")
		}
		r.ForceStop = newForceStop(r.ForceStop, actor, s.now().UTC())
		r.Unsettled = true
		return nil
	})
}

var errForceStopUnchanged = errors.New("force stop state is unchanged")

func (s *Service) writeForceStop(ctx context.Context, id string, revision uint64, actor string, change func(*Record) error) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	var next Record
	_, err = s.l.Transition(ctx, id, string(current.State), string(current.State), actor, nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		if revision != 0 && (next.ForceStop == nil || next.ForceStop.Revision != revision || !forceStopActive(next.ForceStop.Level)) {
			return ErrForceStopChanged
		}
		if _, err := stoppedTaskTx(tx, next); err != nil {
			return err
		}
		if err := change(&next); err != nil {
			return err
		}
		next.Revision = op.Revision + 1
		return setRecordDataTx(tx, op, next)
	})
	if errors.Is(err, errForceStopUnchanged) {
		return s.Get(ctx, id)
	}
	return next, err
}

// RecordForceStopResult persists only changing evidence. Two consecutive
// unanswered calls are durable even if the coordinator changes between them.
func (s *Service) RecordForceStopResult(ctx context.Context, id string, revision uint64, answered bool, code string) (Record, error) {
	return s.writeForceStop(ctx, id, revision, "force-stop", func(r *Record) error {
		if r.ForceStop.Level != "kill" {
			return ErrForceStopChanged
		}
		before := *r.ForceStop
		next := before
		now := s.now().UTC()
		if answered {
			next.UnansweredSince = time.Time{}
			next.UnansweredCount = 0
			switch code {
			case "stop_running":
				if now.Sub(next.LevelSince) >= time.Minute {
					next.Reason = code
				}
			case "unavailable":
				next.Reason = "restart_required"
			case "invalid":
				next.Reason = "upgrade_required"
			case "stop_unproven", "stop_unsupported":
				next.Reason = code
			default:
				next.Reason = "rejected"
			}
		} else {
			if next.UnansweredSince.IsZero() {
				next.UnansweredSince = now
			}
			next.UnansweredCount = min(next.UnansweredCount+1, 2)
			if next.UnansweredCount >= 2 && now.Sub(next.UnansweredSince) >= 30*time.Second {
				next.Reason = "restart_required"
			}
		}
		if next.Reason != "" {
			next.Level = "exhausted"
			next.ExhaustedAt = now
			if next.Reason == "restart_required" {
				next.Level = "restart"
				next.ExhaustedAt = time.Time{}
			}
			next.LevelSince = now
		}
		if next == before {
			return errForceStopUnchanged
		}
		r.ForceStop = &next
		return nil
	})
}

func (s *Service) ConfirmForceStopped(ctx context.Context, id string, revision uint64, proof RetainedEvidence) (Record, error) {
	if revision == 0 {
		return Record{}, ErrForceStopChanged
	}
	if !proof.Session.ProcessStopped {
		return Record{}, ErrStopConfirmationRequired
	}
	return s.confirmTaskStopped(ctx, id, "force-stop", proof, revision)
}

func newForceStop(previous *ForceStop, actor string, now time.Time) *ForceStop {
	revision := uint64(1)
	if previous != nil {
		revision = previous.Revision + 1
	}
	return &ForceStop{Revision: revision, RequestedAt: now, By: actor, Level: "kill", LevelSince: now}
}

// Only an authenticated native exit confirmation calls this projection.
func confirmForceStop(r *Record, now time.Time) {
	if r.ForceStop == nil {
		return
	}
	force := *r.ForceStop
	force.Level, force.LevelSince, force.Reason = "confirmed", now, ""
	force.ExhaustedAt, force.UnansweredSince, force.UnansweredCount = time.Time{}, time.Time{}, 0
	r.ForceStop = &force
}

func forceStopActive(level string) bool {
	return level == "kill" || level == "restart" || level == "await"
}

// AdvanceForceStop moves a still-current request without extending a running
// phase's clock. Repeated observations of the same phase are not writes.
func (s *Service) AdvanceForceStop(ctx context.Context, id string, revision uint64, from, to, reason string) (Record, error) {
	return s.writeForceStop(ctx, id, revision, "force-stop", func(r *Record) error {
		if r.ForceStop.Level != from {
			return ErrForceStopChanged
		}
		if to != "exhausted" && !(from == "restart" && to == "await") {
			return ErrForceStopChanged
		}
		force := *r.ForceStop
		force.Level, force.LevelSince, force.Reason = to, s.now().UTC(), reason
		force.UnansweredSince, force.UnansweredCount = time.Time{}, 0
		if to == "exhausted" {
			force.ExhaustedAt = force.LevelSince
		}
		r.ForceStop = &force
		return nil
	})
}
