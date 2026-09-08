package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/roster"
)

// openAttempt leases a chat turn on the project's canonical workspace. A
// project someone else is editing in place right now is refused with who
// holds it and the two ways out.
func (c *Coordinator) openAttempt(ctx context.Context, req Request, selected agent.Agent, taskID string, binding project.Binding, workspace project.Workspace) (attempt.Record, []acp.MCPServer, error) {
	if c.attempts == nil {
		return attempt.Record{}, nil, errors.New("turn: attempts are not wired")
	}
	var bound []acp.MCPServer
	spec := attempt.Spec{Execution: execution.Token(ctx),
		TaskID: taskID, TurnID: req.MessageID, Kind: attempt.KindChat, Project: binding.ProjectID,
		Node: selected.Node, Harness: selected.Harness, Agent: selected.ID,
		Workspace: workspace, Scope: attempt.ScopeUnrestricted, By: req.SenderOpenID,
	}
	if workspace.Kind == project.KindWorktree {
		spec.Scope = attempt.ScopePathSet
		spec.Base = workspace.Base
	}
	// The machine must qualify for the project's level; the roster knows
	// both the machine's level and the endpoint's session cap.
	var chosen *roster.Candidate
	if c.fleet != nil {
		cand := c.fleet.ForAgent(ctx, selected)
		chosen = &cand
		spec.Slots = cand.Slots
		spec.Region = cand.Region
		if p, ok, perr := c.projects.Get(ctx, binding.ProjectID); perr == nil && ok {
			spec.CanonicalRegion = c.fleet.RegionOf(p.Home.Node)
			if !p.Level.OrDefault().Admits(cand.Level.OrDefault()) {
				return attempt.Record{}, nil, UserError{Text: c.text.T(i18n.ProjectLevel, p.ID, p.Level.OrDefault(), selected.ID, placeLabel(selected.Node), cand.Level.OrDefault(), protocol.CommandProject)}
			}
		}
	}
	spec.Requires = selected.Requires
	record, err := c.attempts.Open(ctx, spec)
	if err == nil && c.tasks != nil && record.Execution != nil {
		if bindErr := c.tasks.BindAttempt(*record.Execution, record.ID, record.TurnID); bindErr != nil {
			_, closeErr := c.attempts.Fail(ctx, record.ID, "turn", "bind task accounting: "+bindErr.Error())
			return attempt.Record{}, nil, errors.Join(bindErr, closeErr)
		}
	}
	if err == nil {
		// A chat turn is admitted like any other attempt: the machine's
		// own word on the agent's requirements, taken now, kept on the
		// record. A refusal ends the turn before a session is opened.
		if chosen != nil {
			adm, bindings, aerr := c.fleet.Admit(ctx, *chosen, selected.Requires, selected.MCPServers, record.ID)
			if aerr != nil {
				_, _ = c.attempts.Fail(ctx, record.ID, "turn", "admission: "+aerr.Error())
				return attempt.Record{}, nil, fmt.Errorf("admission on %s: %w", placeLabel(selected.Node), aerr)
			}
			if adm.Refused() {
				_, _ = c.attempts.Fail(ctx, record.ID, "turn", "admission refused: "+adm.Unmet())
				return attempt.Record{}, nil, UserError{Text: c.text.T(i18n.AdmissionRefused, selected.ID, placeLabel(selected.Node), adm.Unmet())}
			}
			record.Admission = &adm
			bound = roster.ToMCP(bindings)
		}
		// The before-snapshot is the precondition of running in place: what
		// the turn changes is measured against it.
		if workspace.Kind == project.KindWorktree {
			prepared, perr := c.attempts.Advance(ctx, record.ID, attempt.Prepared, "turn", func(next *attempt.Record) { next.Base = workspace.Base; next.Admission = record.Admission })
			if perr != nil {
				return record, nil, perr
			}
			record = prepared
		} else if c.artifacts != nil {
			if p, ok, perr := c.projects.Get(ctx, binding.ProjectID); perr == nil && ok {
				before, _, serr := c.snapshot(ctx, p, workspace, "", record.ID, "before turn "+req.MessageID)
				if serr != nil {
					_, _ = c.attempts.Fail(ctx, record.ID, "turn", "before-snapshot: "+serr.Error())
					return attempt.Record{}, nil, fmt.Errorf("before-snapshot: %w", serr)
				}
				admission := record.Admission
				prepared, aerr := c.attempts.Advance(ctx, record.ID, attempt.Prepared, "turn", func(r *attempt.Record) { r.Base = before.ID; r.Admission = admission })
				if aerr == nil {
					record = prepared
				}
			}
		}
		return record, bound, nil
	}
	var busy attempt.Busy
	if errors.As(err, &busy) {
		holderAgent, holderTask := busy.Holder, "?"
		if holder, herr := c.attempts.Get(ctx, busy.Holder); herr == nil {
			holderAgent, holderTask = holder.Agent, holder.TaskID
		}
		return attempt.Record{}, nil, UserError{Text: c.text.T(i18n.ProjectBusy, binding.ProjectID, holderAgent, holderTask, protocol.CommandProject)}
	}
	return attempt.Record{}, nil, fmt.Errorf("open attempt: %w", err)
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
func (c *Coordinator) closeAttempt(parent context.Context, id string, result Result, turnErr error, spent *turnSpend) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Minute)
	defer cancel()
	// Last of all — after the attempt is closed and the queued landings
	// are done — whoever waits for this turn's end is told.
	if c.afterTurn != nil {
		if record, err := c.attempts.Get(ctx, id); err == nil && record.TaskID != "" {
			defer c.afterTurn(record.TaskID)
		}
	}
	usage := spent.attemptUsage()
	if errors.Is(turnErr, harness.ErrStopUnconfirmed) {
		if err := c.attempts.MarkUnsettled(ctx, id, "turn", turnErr, usage); err != nil {
			return errors.Join(turnErr, err)
		}
		return turnErr
	}
	if c.fleet != nil {
		if record, err := c.attempts.Get(ctx, id); err == nil && record.Admission != nil && len(record.Admission.Bound) > 0 {
			c.fleet.Release(ctx, record.Node, id)
		}
	}
	if turnErr == nil {
		output, err := json.Marshal(result)
		if err != nil {
			return err
		}
		outcome := attempt.Result{Summary: clip(result.Text, 200), Output: output}
		var binding *attempt.NameBinding
		var pending *project.Project
		reject := func(cause error) error {
			return c.attempts.RejectCompletion(ctx, id, "turn", attempt.Completion{Result: outcome, Usage: usage, Binding: binding}, cause)
		}
		record, err := c.attempts.Get(ctx, id)
		if err != nil {
			return reject(fmt.Errorf("read attempt completion: %w", err))
		}
		if c.artifacts != nil && record.Base != "" {
			// The after-snapshot: the turn's change, bound to the turn's
			// name; then whatever delegations queued up lands under a lock
			// this turn no longer holds.
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
			after, changed, serr := c.snapshot(ctx, p, record.Workspace, record.Base, id, "after turn "+record.TurnID)
			if serr != nil {
				log.Printf("turn: attempt %s after-snapshot: %v", id, serr)
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
		if _, err := c.attempts.FinishCompletion(ctx, id, "turn", attempt.Completion{Result: outcome, Usage: usage, Binding: binding}); err != nil {
			return reject(fmt.Errorf("commit attempt %s: %w", id, err))
		}
		if record.Workspace.Kind == project.KindWorktree && record.Execution != nil && c.tasks != nil {
			if tracked, ok := c.tasks.Get(record.TaskID); ok && tracked.RecoveryWorkspace != nil && tracked.RecoveryWorkspace.ID == record.Workspace.ID {
				workspace := *tracked.RecoveryWorkspace
				if outcome.Artifact != "" {
					workspace.Base = outcome.Artifact
				}
				workspace.AttemptID = record.ID
				if err := c.tasks.BindRecoveryWorkspace(*record.Execution, workspace); err != nil {
					return err
				}
			}
		}
		if pending != nil {
			c.landPending(ctx, *pending)
		}
		c.recordDisclosure(ctx, record, result)
		return nil
	}
	if _, err := c.attempts.FailWith(ctx, id, "turn", turnErr.Error(), usage); err != nil {
		return fmt.Errorf("record failed attempt %s: %w", id, err)
	}
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
