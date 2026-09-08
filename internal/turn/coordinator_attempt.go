package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/project"
)

// completion is a chat turn's result as the ledger records it: the
// after-snapshot — the turn's change, bound to the turn's name. A
// *lifecycle.Rejected error carries what there was when assembling it
// failed, so the attempt closes with it.
func (c *Coordinator) completion(ctx context.Context, record attempt.Record, result Result, usage *attempt.Usage, clock *turnClock) (attempt.Completion, *project.Project, error) {
	output, err := json.Marshal(result)
	if err != nil {
		return attempt.Completion{}, nil, err
	}
	outcome := attempt.Result{Summary: clip(result.Text, 200), Output: output}
	var binding *attempt.NameBinding
	var pending *project.Project
	reject := func(cause error) (attempt.Completion, *project.Project, error) {
		return attempt.Completion{}, nil, &lifecycle.Rejected{Completion: attempt.Completion{Result: outcome, Usage: usage, Binding: binding}, Cause: cause}
	}
	if c.artifacts != nil && record.Base != "" {
		if c.projects == nil {
			return reject(errors.New("completion project source is not configured"))
		}
		p, ok, perr := c.projects.Get(ctx, record.Project)
		if perr != nil {
			return reject(fmt.Errorf("read completion project %s: %w", record.Project, perr))
		}
		if !ok {
			return reject(fmt.Errorf("completion project %s: %w", record.Project, project.ErrUnknown))
		}
		after, changed, serr := c.snapshot(ctx, p, record.Workspace, record.Base, record.ID, "after turn "+record.TurnID)
		clock.mark("after")
		if serr != nil {
			log.Printf("turn: attempt %s after-snapshot: %v", record.ID, serr)
			outcome.CaptureError = serr.Error()
		} else if changed {
			outcome.Artifact = after.ID
			name := "steve/" + record.TaskID + "/turn/" + record.TurnID
			current, _, err := c.artifacts.Resolve(ctx, name)
			if err != nil {
				return reject(fmt.Errorf("resolve completion name: %w", err))
			}
			binding = &attempt.NameBinding{Name: name, ExpectedVersion: current.Version}
		}
		if record.Workspace.Kind == project.KindCanonical {
			pending = &p
		}
	}
	return attempt.Completion{Result: outcome, Usage: usage, Binding: binding}, pending, nil
}

// afterCompletion is what follows a committed completion: the recovery
// workspace rebased on the after-snapshot, whatever delegations queued up
// landing under a lock this turn no longer holds, and the disclosure.
func (c *Coordinator) afterCompletion(ctx context.Context, record attempt.Record, result Result, pending *project.Project, clock *turnClock) error {
	clock.mark("finish")
	if record.Workspace.Kind == project.KindWorktree && record.Execution != nil && c.tasks != nil {
		if tracked, ok := c.tasks.Get(record.TaskID); ok && tracked.RecoveryWorkspace != nil && tracked.RecoveryWorkspace.ID == record.Workspace.ID {
			workspace := *tracked.RecoveryWorkspace
			if record.Result != nil && record.Result.Artifact != "" {
				workspace.Base = record.Result.Artifact
			}
			workspace.AttemptID = record.ID
			if err := c.tasks.BindRecoveryWorkspace(*record.Execution, workspace); err != nil {
				return err
			}
		}
	}
	if pending != nil {
		c.landPending(ctx, *pending)
		clock.mark("land")
	}
	c.recordDisclosure(ctx, record, result)
	return nil
}

func clip(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// landPending lands what delegations left queued for the project.
func (c *Coordinator) landPending(ctx context.Context, p project.Project) {
	landed, err := c.artifacts.LandPending(ctx, p)
	if err != nil {
		log.Printf("turn: land pending for %s: %v", p.ID, err)
	}
	for _, l := range landed {
		log.Printf("turn: landing %s of %s into %s: %s (%d paths)", l.ID, l.Artifact, p.ID, l.State, len(l.Paths))
	}
}

// Disclosure is the record that content of a restricted or sealed project
// left through the chat: the only egress this version has. It is a fact
// for the audit, written before the answer is sent.
type Disclosure struct {
	Project string    `json:"project"`
	Level   string    `json:"level"`
	TaskID  string    `json:"task_id,omitempty"`
	Attempt string    `json:"attempt"`
	Turn    string    `json:"turn"`
	Channel string    `json:"channel"`
	Bytes   int       `json:"bytes"`
	By      string    `json:"by,omitempty"`
	At      time.Time `json:"at"`
}

func (c *Coordinator) recordDisclosure(ctx context.Context, record attempt.Record, result Result) {
	if c.projects == nil || result.Text == "" {
		return
	}
	p, ok, err := c.projects.Get(ctx, record.Project)
	if err != nil || !ok {
		return
	}
	// Internal and public content leaving is not a disclosure.
	if level := p.Level.OrDefault(); level != project.LevelRestricted && level != project.LevelSealed {
		return
	}
	d := Disclosure{Project: p.ID, Level: string(p.Level), TaskID: record.TaskID, Attempt: record.ID, Turn: record.TurnID,
		Channel: "feishu", Bytes: len(result.Text), By: record.By, At: time.Now().UTC()}
	if err := c.projects.Disclose(ctx, record.ID, d); err != nil {
		log.Printf("turn: record disclosure for %s: %v", record.ID, err)
	}
}

// snapshot takes a before- or after-snapshot of the workspace a turn runs
// in: the canonical one moves the project's canonical name, a copy moves
// its own head. An empty parent means "from the workspace's last snapshot".
func (c *Coordinator) snapshot(ctx context.Context, p project.Project, ws project.Workspace, parent, by, message string) (artifact.Manifest, bool, error) {
	if ws.Kind == project.KindWorktree {
		if parent == "" {
			parent = ws.Base
		}
		return c.artifacts.Publish(ctx, ws, parent, by, message)
	}
	if ws.Kind == project.KindCopy {
		if parent == "" {
			parent = c.artifacts.HeadOf(ctx, ws.ID)
		}
		return c.artifacts.SnapshotWorkspace(ctx, p, ws, parent, by, message)
	}
	if parent == "" {
		parent = c.artifacts.CanonicalOf(ctx, p.ID)
	}
	return c.artifacts.SnapshotCanonical(ctx, p, parent, by, message)
}
