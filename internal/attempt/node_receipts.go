package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

const nodeReceiptPendingKind = "attempt-node-receipt-pending"

// ErrNodeReceiptTaskDeleted is an exact receipt whose task was deleted with
// its conversation. It cannot be checked against that conversation any more,
// and nothing of the task is left to account or deliver: the receipt may be
// released, but never recorded as new evidence.
var ErrNodeReceiptTaskDeleted = errors.New("attempt node receipt belongs to a deleted task")

// CheckNodeReceiptTx binds a node's original terminal receipt to its committed
// attempt and logical conversation. It does not check current execution grants:
// cleanup of an original result must survive a later task epoch or idle rebind.
// A task proved deleted reports ErrNodeReceiptTaskDeleted, which only release
// paths accept.
func CheckNodeReceiptTx(tx ledger.Reader, record Record, receipt nodewire.SessionReceipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	b := receipt.Binding
	if record.Execution == nil || receipt.SessionID != record.Session || receipt.ContextID != record.NativeContext ||
		receipt.CommandID != InputCommandID(record) || b.AttemptID != record.ID || b.TaskID != record.TaskID ||
		b.NodeID != record.Node || b.ProjectID != record.Project || b.NativeImportID != record.NativeImportID() ||
		b.PluginRuntimeID != record.PluginRuntimeID() || b.ExecutionEpoch != SessionExecutionEpoch(record) ||
		record.Execution.TaskID != record.TaskID || b.TaskEpoch != record.Execution.Epoch {
		return errors.New("attempt node receipt differs from the original execution")
	}
	tracked, found, err := task.GetTx(tx, record.TaskID)
	if err != nil {
		return err
	}
	if !found {
		deleted, err := task.DeletedTx(tx, record.TaskID)
		if err != nil {
			return err
		}
		if deleted {
			return ErrNodeReceiptTaskDeleted
		}
	}
	if !found || b.SessionID != RetainedSessionID(tracked.Channel, record.TaskID, record.Agent) {
		return errors.New("attempt node receipt differs from the original conversation")
	}
	return nil
}

func recordNodeReceiptTx(tx *ledger.Tx, before, next Record) error {
	if before.NodeReceipt != nil {
		if next.NodeReceipt == nil || *before.NodeReceipt != *next.NodeReceipt {
			return errors.New("attempt node receipt cannot be replaced")
		}
		return nil
	}
	if next.NodeReceipt == nil {
		return nil
	}
	if !next.State.Terminal() || next.Unsettled || next.SessionSettled == nil || !*next.SessionSettled {
		return errors.New("attempt node receipt requires committed terminal settlement")
	}
	if err := CheckNodeReceiptTx(tx, next, *next.NodeReceipt); err != nil {
		return err
	}
	return tx.PutBinding(nodeReceiptPendingKind, next.ID, next.NodeReceipt)
}

// PendingNodeReceipts reads a bounded page from the bindings (kind,id) primary
// index. Closed attempts and acknowledged receipts are never scanned.
func (s *Service) PendingNodeReceipts(ctx context.Context, after string, limit int) ([]nodewire.SessionReceipt, error) {
	if limit < 1 || limit > 128 {
		return nil, errors.New("node receipt pending page must contain 1..128 records")
	}
	rows, err := s.l.DB().QueryContext(ctx, `SELECT id,data FROM bindings WHERE kind=? AND id>? ORDER BY id LIMIT ?`,
		nodeReceiptPendingKind, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []nodewire.SessionReceipt
	for rows.Next() {
		var id string
		var raw []byte
		var receipt nodewire.SessionReceipt
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return nil, fmt.Errorf("decode pending node receipt: %w", err)
		}
		if err := receipt.Validate(); err != nil {
			return nil, err
		}
		if receipt.Binding.AttemptID != id {
			return nil, errors.New("pending node receipt identity differs")
		}
		pending = append(pending, receipt)
	}
	return pending, rows.Err()
}

// AcknowledgeNodeReceipt consumes only the pending retry index after the
// runtime has received an exact authenticated node acknowledgement. Durable
// result/receipt evidence stays on the attempt; this is not an external API.
func (s *Service) AcknowledgeNodeReceipt(ctx context.Context, receipt nodewire.SessionReceipt) error {
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		record, err := GetTx(tx, receipt.Binding.AttemptID)
		if err != nil {
			return err
		}
		if record.NodeReceipt == nil || *record.NodeReceipt != receipt || !record.State.Terminal() || record.Unsettled {
			return errors.New("node acknowledgement differs from durable terminal evidence")
		}
		if err := CheckNodeReceiptTx(tx, record, receipt); err != nil && !errors.Is(err, ErrNodeReceiptTaskDeleted) {
			return err
		}
		var raw []byte
		err = tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, nodeReceiptPendingKind, record.ID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var pending nodewire.SessionReceipt
		if json.Unmarshal(raw, &pending) != nil || pending != receipt {
			return errors.New("pending acknowledgement identity differs")
		}
		_, err = tx.Exec(`DELETE FROM bindings WHERE kind=? AND id=?`, nodeReceiptPendingKind, record.ID)
		return err
	})
}
