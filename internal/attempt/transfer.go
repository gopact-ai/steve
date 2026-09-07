package attempt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
)

func (s *Service) ExportProject(ctx context.Context, project string) (ledger.TransferFacts, error) {
	ops, err := s.l.Operations(ctx, kind, "")
	if err != nil {
		return ledger.TransferFacts{}, err
	}
	var ids []string
	for _, op := range ops {
		r, err := decode(op)
		if err != nil {
			return ledger.TransferFacts{}, err
		}
		if r.Project == project {
			ids = append(ids, r.ID)
		}
	}
	return s.l.ExportOperations(ctx, ids)
}
func ReleaseProjectGuard(tx *ledger.Tx, project string) error {
	ops, err := tx.Operations(kind, "")
	if err != nil {
		return err
	}
	for _, op := range ops {
		r, err := decode(op)
		if err != nil {
			return err
		}
		if r.Project == project && potentialWriter(r) {
			return fmt.Errorf("project %s has active/unknown writer %s; verify physical stop first", project, r.ID)
		}
	}
	return nil
}

func RemapTransfer(f *ledger.TransferFacts, m ledger.TransferIDs) error {
	for i := range f.Operations {
		op := &f.Operations[i]
		if op.Kind != kind {
			return fmt.Errorf("invalid attempt transfer kind %s", op.Kind)
		}
		r, err := decode(*op)
		if err != nil {
			return err
		}
		r.TaskID = m.Task(r.TaskID)
		// Source fences remain in historical events, never as target authority.
		r.Leases = nil
		if r.Execution != nil {
			r.Execution.TaskID = m.Task(r.Execution.TaskID)
		}
		if r.Kind == KindStep {
			before, after, ok := strings.Cut(r.TurnID, "/")
			if ok {
				r.TurnID = m.Plan(before) + "/" + after
			}
		} else if r.Kind == KindDelegate {
			r.TurnID = "delegate/" + r.TaskID
		}
		if r.Result != nil {
			for j, ref := range r.Result.Refs {
				r.Result.Refs[j] = ledger.RemapTaskReference(ref, m.Task)
			}
		}
		op.Data, err = json.Marshal(r)
		if err != nil {
			return err
		}
	}
	f.RemapEnvelopes(m)
	return nil
}
