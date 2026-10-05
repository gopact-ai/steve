package exec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

func (r RunRecord) checkIdentity() error {
	if r.PlanID == "" || r.ID != planRunID(r.PlanID) || r.Execution != nil && (r.TaskID == "" || r.Execution.TaskID != r.TaskID) {
		return fmt.Errorf("%w: invalid plan run ownership %s", ErrRecovery, r.ID)
	}
	for _, sink := range r.Sinks {
		if sink.Source != nil && (sink.Source.Execution == nil || r.Execution == nil || *sink.Source.Execution != *r.Execution) {
			return fmt.Errorf("%w: plan run %s sink %s has different execution ownership", ErrRecovery, r.ID, sink.StepID)
		}
	}
	switch r.Phase {
	case RunExecuting, RunLanding, RunCompleted:
		return nil
	default:
		return fmt.Errorf("%w: plan run %s has unknown phase %s", ErrRecovery, r.ID, r.Phase)
	}
}

func (r RunRecord) checkPlan(p plan.Plan) error {
	if err := r.checkIdentity(); err != nil {
		return err
	}
	if r.PlanID != p.ID || r.TaskID != p.TaskID || r.ProjectID != p.ProjectID || p.Execution != nil && (r.Execution == nil || *r.Execution != *p.Execution) {
		return fmt.Errorf("%w: plan run %s differs from its plan owner", ErrRecovery, r.ID)
	}
	return nil
}

func (r RunRecord) sameOwner(other RunRecord) bool {
	tokensEqual := r.Execution == nil && other.Execution == nil || r.Execution != nil && other.Execution != nil && *r.Execution == *other.Execution
	return r.ID == other.ID && r.PlanID == other.PlanID && r.TaskID == other.TaskID && r.ProjectID == other.ProjectID && tokensEqual
}

func decodeRun(op ledger.Operation) (RunRecord, error) {
	var r RunRecord
	if op.Kind != runKind {
		return r, fmt.Errorf("%w: operation %s is not a plan run", ErrRecovery, op.ID)
	}
	if err := json.Unmarshal(op.Data, &r); err != nil {
		return r, err
	}
	if r.ID != op.ID || r.Phase != op.State {
		return r, fmt.Errorf("%w: invalid plan run envelope %s", ErrRecovery, op.ID)
	}
	return r, r.checkIdentity()
}

func (s *Supervisor) checkRunAdmission(ctx context.Context, r RunRecord, p plan.Plan) error {
	// Match Registry.BeginAccepted before Begin can leave a durable record.
	if inherited := execution.Token(ctx); inherited != nil && inherited.TaskID == r.TaskID && (r.Execution == nil || *inherited != *r.Execution) {
		return task.ErrExecutionStopped
	}
	if err := r.checkPlan(p); err != nil {
		return err
	}
	if s.plans == nil {
		return nil
	}
	stored, found := s.plans.Latest(r.PlanID)
	if !found {
		return fmt.Errorf("%w: plan %s vanished", ErrRecovery, r.PlanID)
	}
	return r.checkPlan(stored)
}
