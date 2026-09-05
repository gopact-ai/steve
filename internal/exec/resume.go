package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	gopact "github.com/gopact-ai/gopact"
	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
)

// RunRecord is a plan run the ledger knows about: opened when execution
// starts, closed when it ends. One left open by a hub that died is what
// ResumeAll picks back up.
type RunRecord struct {
	PlanID   string    `json:"plan_id"`
	Rev      int       `json:"rev"`
	TaskID   string    `json:"task_id"`
	RunID    string    `json:"run_id"`
	OpenedAt time.Time `json:"opened_at"`
	Owner    string    `json:"owner"`
}

const runKind = "plan-run"

// runIDFor names a revision's run so a restarted hub can find its
// checkpoints: the id is a fact of the plan, not of the process.
func runIDFor(p plan.Plan) string { return fmt.Sprintf("plan-%s-r%d", p.ID, p.Rev) }

// SetLedger records open runs in the ledger so they survive the process.
func (s *Supervisor) SetLedger(l *ledger.Ledger, owner string) {
	s.ledger = l
	s.owner = owner
}

func (s *Supervisor) opened(ctx context.Context, p plan.Plan) {
	if s.ledger == nil {
		return
	}
	rec := RunRecord{PlanID: p.ID, Rev: p.Rev, TaskID: p.TaskID, RunID: runIDFor(p), OpenedAt: time.Now().UTC(), Owner: s.owner}
	if err := s.ledger.PutBinding(ctx, runKind, p.ID, rec); err != nil {
		log.Printf("exec: record run of plan %s: %v", p.ID, err)
	}
}

func (s *Supervisor) closed(ctx context.Context, p plan.Plan) {
	if s.ledger == nil {
		return
	}
	if err := s.ledger.DeleteBinding(context.WithoutCancel(ctx), runKind, p.ID); err != nil {
		log.Printf("exec: close run of plan %s: %v", p.ID, err)
	}
}

// OpenRuns lists plan runs a previous process left open.
func (s *Supervisor) OpenRuns(ctx context.Context) ([]RunRecord, error) {
	if s.ledger == nil {
		return nil, nil
	}
	raw, err := s.ledger.Bindings(ctx, runKind)
	if err != nil {
		return nil, err
	}
	var out []RunRecord
	for _, data := range raw {
		var rec RunRecord
		if err := json.Unmarshal(data, &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Resume continues an open run from its last checkpoint. Steps already
// done are not rerun; a step that was in flight is retried, which takes
// over the attempt the dead process left behind. Revisions after that
// work as in Execute.
func (s *Supervisor) Resume(ctx context.Context, rec RunRecord) (Outcome, error) {
	if s.plans == nil {
		return Outcome{}, fmt.Errorf("no plan store to resume plan %s from", rec.PlanID)
	}
	current, ok := s.plans.Latest(rec.PlanID)
	if !ok {
		s.closed(ctx, plan.Plan{ID: rec.PlanID})
		return Outcome{}, fmt.Errorf("plan %s vanished", rec.PlanID)
	}
	defer s.closed(ctx, current)
	outcome, err := s.runs.Resume(ctx, current, s.deps, rec.RunID)
	if err == nil || ctx.Err() != nil || s.plans == nil || !NeedsRevision(err) {
		return outcome, err
	}
	// From here the ordinary loop: revise and run the new revision.
	return s.continueFrom(ctx, current, outcome, err, 1)
}

// Resume continues a plan after the process that ran it is gone. The
// workflow's own checkpoint is tried first; when the runtime will not take
// it back — a checkpoint left canceled by a graceful shutdown, or still
// leased to the dead owner — the plan is run again under a new run id.
// That is safe because the plan store, not the checkpoint, is the record
// of what finished: steps already done pass straight through, and a step
// that was in flight is retried as a takeover of its abandoned attempt.
func (r *Runs) Resume(ctx context.Context, p plan.Plan, deps Deps, runID string) (Outcome, error) {
	records, err := r.store.ListCheckpoints(ctx, workflow.CheckpointHistoryRequest{RunID: runID, Limit: 10000})
	if err != nil {
		return Outcome{}, fmt.Errorf("checkpoints of %s: %w", runID, err)
	}
	if len(records) > 0 {
		last := records[len(records)-1]
		for _, rec := range records {
			if rec.Version > last.Version {
				last = rec
			}
		}
		outcome, err := r.run(ctx, p, deps, workflow.WithResume(workflow.ResumeRequest{RunID: runID, CheckpointID: last.ID}))
		if err == nil || !notResumable(err) {
			return outcome, err
		}
		log.Printf("exec: run %s not resumable from its checkpoint (%v); rerunning the revision with finished steps kept", runID, err)
	}
	return r.run(ctx, p, deps, gopact.WithRunID(fmt.Sprintf("%s-again-%d", runID, time.Now().UnixNano())))
}

// notResumable is the runtime refusing a checkpoint, as opposed to the plan
// failing: the latter must not be retried by rerunning.
func notResumable(err error) bool {
	text := err.Error()
	return strings.Contains(text, "cannot resume") || strings.Contains(text, "checkpoint version conflict") ||
		strings.Contains(text, "lease") || strings.Contains(text, "checkpoint not found")
}
