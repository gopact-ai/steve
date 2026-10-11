package turn

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/view"
)

type recoveryWaiting struct{ id string }

func (e recoveryWaiting) Error() string {
	return "workspace recovery " + e.id + " is finalizing; input remains waiting"
}
func (e recoveryWaiting) Unwrap() error { return attempt.ErrWorkspaceRecovery }

func (c *Coordinator) awaitRecoveryWorkspace(ctx context.Context, req Request, selected agent.Agent, binding project.Binding, touch func()) (project.Workspace, error) {
	for {
		ws, err := c.workspaceFor(ctx, req, selected, binding)
		var waiting recoveryWaiting
		if !errors.As(err, &waiting) {
			return ws, err
		}
		req.stage(view.StageAwaitSnapshot)
		if touch != nil {
			touch()
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return project.Workspace{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// recoveryAdmissionAttempts refreshes only an input whose Open was refused
// before admission. It cannot replay an attempt, task charge or native prompt.
type recoveryAdmissionAttempts struct {
	waitingAttempts
	turn *chatTurn
}

func (a recoveryAdmissionAttempts) Open(ctx context.Context, spec attempt.Spec) (attempt.Record, error) {
	if spec.ID == "" {
		spec.ID = attempt.NewID()
	}
	refreshedWithoutEpisode := false
	for {
		record, err := a.waitingAttempts.Open(ctx, spec)
		if err == nil || record.ID != "" || !errors.Is(err, attempt.ErrWorkspaceRecovery) {
			return record, err
		}
		if spec.WorkspaceRecovery == nil {
			if spec.Workspace.Kind != project.KindCanonical || spec.Project != a.turn.binding.ProjectID {
				return record, err
			}
			_, active, readErr := a.turn.c.attempts.RecoveryForProject(ctx, spec.Project)
			if readErr != nil {
				return record, readErr
			}
			// Release may have committed between the refusal and this read.
			// Refresh once without an active episode, never loop on an
			// unexplained repeated refusal.
			if !active && refreshedWithoutEpisode {
				return record, err
			}
			refreshedWithoutEpisode = !active
		} else {
			episode, readErr := a.turn.c.attempts.WorkspaceRecovery(ctx, spec.WorkspaceRecovery.ID)
			if readErr != nil {
				return record, readErr
			}
			if episode.Phase != "draining" && episode.Phase != "capture" && episode.Phase != "landing" && episode.Phase != "released" {
				return record, err
			}
		}
		ws, err := a.turn.c.awaitRecoveryWorkspace(ctx, a.turn.req, a.turn.selected, a.turn.binding, a.turn.spent.resetIdle)
		if err != nil {
			return record, err
		}
		a.turn.workspace = ws
		if err := a.turn.prepareSession(ctx); err != nil {
			return record, err
		}
		fresh, candidate, err := a.turn.c.turnSpec(ctx, a.turn.req, a.turn.selected, a.turn.tracked, a.turn.binding, ws)
		if err != nil {
			return record, err
		}
		fresh.ID, fresh.PluginRuntime, fresh.NativeImport = spec.ID, a.turn.saved.PluginRuntime.Clone(), a.turn.saved.NativeImport.Clone()
		if fresh.Execution == nil {
			return record, errors.New("waiting recovery input lost its execution token")
		}
		if err := a.turn.c.tasks.SetPendingTurnWorkspace(ctx, *fresh.Execution, fresh.TurnID, ws.Path); err != nil {
			return record, err
		}
		if a.turn.req.Admission.ExpectedTask == "" {
			if fresh.WorkspaceRecovery == nil {
				a.waitingAttempts.passes = snapshotPasses
				a.waitingAttempts.limit = snapshotWaitLimit
			} else {
				a.waitingAttempts.passes = recoveryCopyPasses(fresh.Workspace.ID)
				a.waitingAttempts.limit = 0
			}
		}
		a.turn.recoveryRefreshed, a.turn.recoveryCandidate = true, candidate
		spec = fresh
	}
}
