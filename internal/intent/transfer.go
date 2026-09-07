package intent

import (
	"context"
	"encoding/json"

	"github.com/gopact-ai/steve/internal/ledger"
)

func (s *Service) ExportTasks(ctx context.Context, tasks map[string]bool) (ledger.TransferFacts, error) {
	ops, err := s.l.Operations(ctx, kind, "")
	if err != nil {
		return ledger.TransferFacts{}, err
	}
	var ids []string
	for _, op := range ops {
		var in Intent
		if err := json.Unmarshal(op.Data, &in); err != nil {
			return ledger.TransferFacts{}, err
		}
		if tasks[in.TaskID] {
			ids = append(ids, op.ID)
		}
	}
	return s.l.ExportOperations(ctx, ids)
}

func RemapTransfer(f *ledger.TransferFacts, m ledger.TransferIDs) error {
	for i := range f.Operations {
		op := &f.Operations[i]
		var value Intent
		if err := json.Unmarshal(op.Data, &value); err != nil {
			return err
		}
		value.ID = m.Operation(value.ID)
		value.TaskID = m.Task(value.TaskID)
		if value.State == Unknown || value.State == Dispatched {
			value.RequiresReconciliation = true
		}
		op.Data, _ = json.Marshal(value)
	}
	f.RemapEnvelopes(m)
	return nil
}
