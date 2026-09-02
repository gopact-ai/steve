package turn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

// openAttempt leases a chat turn on the project's canonical workspace. A
// project someone else is editing in place right now is refused with who
// holds it and the two ways out.
func (c *Coordinator) openAttempt(ctx context.Context, req Request, selected agent.Agent, taskID string, binding project.Binding, workspace project.Workspace) (attempt.Record, error) {
	if c.attempts == nil {
		return attempt.Record{}, errors.New("turn: attempts are not wired")
	}
	spec := attempt.Spec{
		TaskID: taskID, TurnID: req.MessageID, Kind: attempt.KindChat, Project: binding.ProjectID,
		Node: selected.Node, Harness: selected.Harness, Agent: selected.ID,
		Workspace: workspace, Scope: attempt.ScopeUnrestricted, By: req.SenderOpenID,
	}
	record, err := c.attempts.Open(ctx, spec)
	if err == nil {
		// The before-snapshot is the precondition of running in place: what
		// the turn changes is measured against it.
		if c.artifacts != nil {
			if p, ok, perr := c.projects.Get(ctx, binding.ProjectID); perr == nil && ok {
				before, _, serr := c.artifacts.SnapshotCanonical(ctx, p, c.artifacts.CanonicalOf(ctx, p.ID), record.ID, "before turn "+req.MessageID)
				if serr != nil {
					_, _ = c.attempts.Fail(ctx, record.ID, "turn", "before-snapshot: "+serr.Error())
					return attempt.Record{}, fmt.Errorf("before-snapshot: %w", serr)
				}
				prepared, aerr := c.attempts.Advance(ctx, record.ID, attempt.Prepared, "turn", func(r *attempt.Record) { r.Base = before.ID })
				if aerr == nil {
					record = prepared
				}
			}
		}
		return record, nil
	}
	var busy attempt.Busy
	if errors.As(err, &busy) {
		holderAgent, holderTask := busy.Holder, "?"
		if holder, herr := c.attempts.Get(ctx, busy.Holder); herr == nil {
			holderAgent, holderTask = holder.Agent, holder.TaskID
		}
		return attempt.Record{}, UserError{Text: c.text.T(i18n.ProjectBusy, binding.ProjectID, holderAgent, holderTask, protocol.CommandProject)}
	}
	return attempt.Record{}, fmt.Errorf("open attempt: %w", err)
}

// advanceAttempt moves the turn's attempt; a refusal here means the lease
// is gone and the turn is already being cancelled, so it is logged, not
// raised.
func (c *Coordinator) advanceAttempt(ctx context.Context, id string, to attempt.State) {
	if _, err := c.attempts.Advance(ctx, id, to, "turn", nil); err != nil {
		log.Printf("turn: attempt %s → %s: %v", id, to, err)
	}
}

// closeAttempt records the outcome even when the turn's own context is
// gone: a cancelled turn is still a fact.
func (c *Coordinator) closeAttempt(parent context.Context, id string, result Result, turnErr error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Minute)
	defer cancel()
	if turnErr == nil {
		outcome := attempt.Result{Summary: clip(result.Text, 200)}
		record, err := c.attempts.Get(ctx, id)
		if err == nil && c.artifacts != nil && record.Base != "" {
			// The after-snapshot: the turn's change, bound to the turn's
			// name; then whatever delegations queued up lands under a lock
			// this turn no longer holds.
			if p, ok, perr := c.projects.Get(ctx, record.Project); perr == nil && ok {
				after, changed, serr := c.artifacts.SnapshotCanonical(ctx, p, record.Base, id, "after turn "+record.TurnID)
				if serr != nil {
					log.Printf("turn: attempt %s after-snapshot: %v", id, serr)
				} else if changed {
					outcome.Artifact = after.ID
					name := "steve/" + record.TaskID + "/turn/" + record.TurnID
					current, _, _ := c.artifacts.Resolve(ctx, name)
					if _, err := c.artifacts.Bind(ctx, name, current.Version, after.ID); err != nil {
						log.Printf("turn: bind %s: %v", name, err)
					}
				}
				defer c.landPending(ctx, p)
			}
		}
		if _, err := c.attempts.Finish(ctx, id, "turn", outcome); err != nil {
			log.Printf("turn: attempt %s finish: %v", id, err)
		}
		return
	}
	if _, err := c.attempts.Fail(ctx, id, "turn", turnErr.Error()); err != nil {
		log.Printf("turn: attempt %s fail: %v", id, err)
	}
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
