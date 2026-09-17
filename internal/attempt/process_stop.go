package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ConfirmProcessStopped ends an execution whose node reports that the process
// behind it is gone while the command itself never said how it finished. The
// node is the only authority on its own processes, so this receipt settles a
// physical fact, not the work: the attempt ends as failed, its quarantine
// clears and its leases retire, so the task can be taken up again instead of
// waiting on a node that has nothing left to say.
func (s *Service) ConfirmProcessStopped(ctx context.Context, id, actor string, proof RetainedEvidence) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if taskStopAlreadySettled(current) {
		return current, nil
	}
	if actor == "" || proof.ObservedAt.IsZero() || proof.ObservedAt.After(s.now().Add(time.Second)) || s.now().Sub(proof.ObservedAt) > time.Minute {
		return Record{}, errors.New("process stop requires fresh native evidence and an actor")
	}
	if !proof.Session.ProcessStopped {
		return Record{}, ErrStopConfirmationRequired
	}
	to := current.State
	if !to.Terminal() {
		to = Failed
	}
	var next Record
	_, err = s.l.Transition(ctx, id, string(current.State), string(to), actor, nil, map[string]string{"stop_evidence": "process-stop/" + id}, func(tx *ledger.Tx, op *ledger.Operation) error {
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		if taskStopAlreadySettled(next) {
			return errTaskStopRecorded
		}
		tracked, err := nativeTaskTx(tx, next)
		if err != nil {
			return err
		}
		st := proof.Session
		if !matchesStoppedSession(next, tracked, st) {
			return errors.New("native stop receipt belongs to another execution")
		}
		if st.Command != nil && st.Command.Settled {
			// A command that reported its own end is delivered, never
			// rewritten into an interruption.
			return errors.New("original native command is settled and must be delivered")
		}
		if err := tx.PutBinding(taskStopReceiptKind, next.ID, TaskStopReceipt{AttemptID: next.ID, Kind: "native-process-stopped", Evidence: proof}); err != nil {
			return err
		}
		if err := tx.RetireStopped(next.Leases); err != nil {
			return err
		}
		settled := true
		next.Unsettled, next.SessionSettled = false, &settled
		next.StopEvidence = "process-stop/" + next.ID
		next.State, next.Revision = to, op.Revision+1
		next.Error = "原执行的进程已在节点上停止，这次执行没有留下完成回执，需要时可以重新执行。"
		if next.EndedAt.IsZero() {
			next.EndedAt = s.now().UTC()
		}
		if spend := stoppedUsage(st); spend != nil {
			next.Usage = spend
		}
		return tx.SetData(op, next)
	})
	if errors.Is(err, errTaskStopRecorded) {
		return s.Get(ctx, id)
	}
	if err != nil {
		return Record{}, err
	}
	for _, lease := range current.Leases {
		if err := s.l.ReleaseAny(ctx, lease); err != nil && !errors.Is(err, ledger.ErrStale) {
			return next, err
		}
	}
	return next, nil
}
