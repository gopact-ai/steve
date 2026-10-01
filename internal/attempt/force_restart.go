package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"errors"
	"github.com/gopact-ai/steve/internal/ledger"
	"time"
)

// ForceRestart is the latest durable restart of one machine. Its ID is never
// reused: a delayed request for a replaced operation is refused.
type ForceRestart struct {
	ID          string    `json:"id"`
	ClusterID   string    `json:"cluster_id"`
	NodeID      string    `json:"node_id"`
	Holder      string    `json:"holder"`
	By          string    `json:"by"`
	RequestedAt time.Time `json:"requested_at"`
	ClaimedAt   time.Time `json:"claimed_at,omitempty"`
	PlanID      string    `json:"plan_id,omitempty"`
	Kind        string    `json:"kind,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	Outcome     string    `json:"outcome,omitempty"`
}

var ErrForceRestartChanged = errors.New("member restart identity changed")

const forceRestartKind = "force-stop-member-restart"

// BeginForceRestart joins an existing node operation or reserves one atomically
// with the attempt's phase. A terminal operation is reused by older requests;
// only a later explicit owner request can replace it.
func (s *Service) BeginForceRestart(ctx context.Context, id string, revision uint64, clusterID, holder string) (ForceRestart, bool, error) {
	var op ForceRestart
	fresh := false
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := GetTx(tx, id)
		if err != nil {
			return err
		}
		if r.ForceStop == nil || r.ForceStop.Revision != revision {
			return ErrForceStopChanged
		}
		if r.ForceStop.Level == "restart" && r.ForceStop.RestartID != "" {
			op, _, err = forceRestartTx(tx, r.Node)
			if err == nil && op.ID != r.ForceStop.RestartID {
				return ErrForceRestartChanged
			}
			return err
		}
		if r.ForceStop.Level != "restart" || r.ForceStop.Reason != "restart_required" || clusterID == "" || holder == "" {
			return ErrForceStopChanged
		}
		if _, err := stoppedTaskTx(tx, r); err != nil {
			return err
		}
		var found bool
		op, found, err = forceRestartTx(tx, r.Node)
		if err != nil {
			return err
		}
		if found && op.ClusterID != clusterID {
			return ErrForceRestartChanged
		}
		if found && op.FinishedAt.IsZero() && s.now().Sub(op.RequestedAt) >= 7*time.Minute {
			op.FinishedAt, op.Outcome = op.RequestedAt.Add(7*time.Minute), "timeout"
			if err := tx.PutBinding(forceRestartKind, r.Node, op); err != nil {
				return err
			}
		}
		if !found || !op.FinishedAt.IsZero() && r.ForceStop.RequestedAt.After(op.FinishedAt) {
			op = ForceRestart{ID: fmt.Sprintf("%s/%d", id, revision), ClusterID: clusterID, NodeID: r.Node, Holder: holder, By: r.ForceStop.By, RequestedAt: s.now().UTC()}
			if err := tx.PutBinding(forceRestartKind, r.Node, op); err != nil {
				return err
			}
			fresh = true
		}
		force := *r.ForceStop
		force.Level, force.LevelSince, force.Reason = "restart", op.RequestedAt, ""
		force.ExhaustedAt = time.Time{}
		force.RestartID, force.RestartHolder, force.RestartRequestedAt = op.ID, op.Holder, op.RequestedAt
		r.ForceStop = &force
		return writeForceRestartAttempt(tx, r)
	})
	return op, fresh, err
}

func writeForceRestartAttempt(tx *ledger.Tx, r Record) error {
	before, err := json.Marshal(r)
	if err != nil {
		return err
	}
	op := ledger.Operation{ID: r.ID, Kind: "attempt", State: string(r.State), Revision: r.Revision, Data: before}
	r.Revision++
	if err := setRecordDataTx(tx, &op, r); err != nil {
		return err
	}
	return tx.RecordTransition(op, string(r.State), "force-stop")
}

func forceRestartTx(tx ledger.Reader, node string) (ForceRestart, bool, error) {
	var op ForceRestart
	var raw string
	err := tx.QueryRow("SELECT data FROM bindings WHERE kind=? AND id=?", forceRestartKind, node).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return op, false, nil
	}
	if err != nil {
		return op, false, err
	}
	err = json.Unmarshal([]byte(raw), &op)
	return op, true, err
}

func (s *Service) ForceRestart(ctx context.Context, node string) (ForceRestart, bool, error) {
	var op ForceRestart
	found, err := s.l.GetBinding(ctx, forceRestartKind, node, &op)
	return op, found, err
}

// ClaimForceRestart reserves the one right to start SSH, after the holder has
// taken its local machine slot. Authorization reads share this transaction.
// A committed claim is never replayed, even if its answer was lost.
func (s *Service) ClaimForceRestart(ctx context.Context, request ForceRestart, plan, kind string, authorize func(ledger.Reader, ForceRestart) error) (bool, error) {
	claimed := false
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		op, found, err := forceRestartTx(tx, request.NodeID)
		if err != nil {
			return err
		}
		if !found || !sameRestart(op, request) || plan == "" || kind != "restart" && kind != "upgrade" || authorize == nil {
			return ErrForceRestartChanged
		}
		if err := authorize(tx, op); err != nil {
			return err
		}
		if !op.ClaimedAt.IsZero() {
			return nil
		}
		if !op.FinishedAt.IsZero() || s.now().Sub(op.RequestedAt) >= 7*time.Minute {
			return ErrForceRestartChanged
		}
		op.ClaimedAt, op.PlanID, op.Kind = s.now().UTC(), plan, kind
		if err := tx.PutBinding(forceRestartKind, op.NodeID, op); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}

func sameRestart(a, b ForceRestart) bool {
	return a.ID == b.ID && a.NodeID == b.NodeID && a.ClusterID == b.ClusterID && a.Holder == b.Holder && a.By == b.By && a.RequestedAt.Equal(b.RequestedAt)
}

func (s *Service) FinishForceRestart(ctx context.Context, request ForceRestart, outcome string) error {
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		op, found, err := forceRestartTx(tx, request.NodeID)
		if err != nil {
			return err
		}
		if !found || !sameRestart(op, request) {
			return ErrForceRestartChanged
		}
		if !op.FinishedAt.IsZero() {
			return nil
		}
		op.FinishedAt, op.Outcome = s.now().UTC(), outcome
		return tx.PutBinding(forceRestartKind, op.NodeID, op)
	})
}
