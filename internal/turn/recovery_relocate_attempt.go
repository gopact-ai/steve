// The replacement attempt a relocation runs as: how the target is chosen
// and verified, how the original execution is inspected, and how the new
// attempt is opened, bound and given a frozen session to run in.

package turn

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"log/slog"
	"strings"
	"time"
)

func (c *Coordinator) inspectRelocation(ctx context.Context, r attempt.Record) (*attempt.RetainedEvidence, error) {
	manager, ok := c.runtime.(retainedRuntime)
	if !ok {
		return nil, errors.New("node inspection is not available")
	}
	ctx = execution.WithProbeKey(ctx, execution.Key{TaskID: r.TaskID, InstanceID: r.TurnID, AttemptID: r.ID})
	runner, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: r.Node, Harness: r.Harness}, r.Session, r.Workspace.Path)
	if err != nil {
		return nil, err
	}
	inspector, ok := runner.(harness.RetainedSessionInspector)
	if !ok {
		return nil, errors.New("node inspector is not available")
	}
	snapshot, err := inspector.InspectRetained(ctx)
	if err != nil {
		return nil, err
	}
	return &attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: snapshot}, nil
}

func undispatchedStopped(r attempt.Record, p *attempt.RetainedEvidence, logicalSession string) bool {
	if p == nil || p.Session.Command == nil || p.ObservedAt.IsZero() || p.ObservedAt.After(time.Now().Add(time.Second)) || time.Since(p.ObservedAt) > time.Minute {
		return false
	}
	s, cmd := p.Session, p.Session.Command
	return s.ID == r.Session && s.Harness == r.Harness && s.Binding.AttemptID == r.ID && s.Binding.SessionID == logicalSession &&
		s.Binding.TaskID == r.TaskID && s.Binding.ProjectID == r.Project && s.Binding.NodeID == r.Node &&
		r.Execution != nil && s.Binding.TaskEpoch == r.Execution.Epoch && s.Binding.ExecutionEpoch == attempt.SessionExecutionEpoch(r) &&
		s.ProcessStopped && cmd.ProcessStopped && cmd.ID == attempt.InputCommandID(r) &&
		cmd.DispatchState == "not-dispatched" && cmd.InputSequence > 0 && cmd.InputSequence <= s.InputAccepted &&
		!cmd.Settled && !cmd.CancelRequested && cmd.State != nodewire.SessionCommandCancelled
}

func (c *Coordinator) relocationTarget(ctx context.Context, original attempt.Record, nodeID string) (agent.Agent, roster.Candidate, error) {
	if c.fleet == nil {
		return agent.Agent{}, roster.Candidate{}, errors.New("可用节点目录尚未就绪")
	}
	configured, ok := c.catalog.Resolve(original.Agent)
	if !ok {
		return agent.Agent{}, roster.Candidate{}, errors.New("原Agent配置已不存在")
	}
	if original.Preferences != nil {
		configured.Model, configured.Options = original.Preferences.Model, original.Preferences.Options
	} else if tracked, ok := c.tasks.Get(original.TaskID); ok {
		configured.Model, configured.Options = c.preferred(tracked.Channel, configured)
	}
	var reasons []string
	for _, candidate := range c.fleet.All(ctx) {
		if candidate.Node == "" || candidate.Node == original.Node || (nodeID != "" && candidate.Node != nodeID) || candidate.Harness != original.Harness {
			continue
		}
		if !candidate.Eligible || !candidate.Up {
			reasons = append(reasons, candidate.Node+": "+candidate.Why)
			continue
		}
		if configured.Model != "" && len(candidate.Models) > 0 {
			found := false
			for _, model := range candidate.Models {
				if model == configured.Model || original.Preferences != nil && original.Preferences.ModelLabel != "" && model == original.Preferences.ModelLabel {
					found = true
				}
			}
			for _, selector := range candidate.Selectors {
				if selector.Category != "model" {
					continue
				}
				for _, value := range selector.Values {
					if value == configured.Model {
						found = true
					}
				}
			}
			if !found {
				reasons = append(reasons, candidate.Node+": 缺少指定模型 "+configured.Model)
				continue
			}
		}
		p, ok, err := c.projects.Get(ctx, original.Project)
		if err != nil {
			return agent.Agent{}, roster.Candidate{}, err
		}
		if !ok {
			return agent.Agent{}, roster.Candidate{}, errors.New("项目记录缺失")
		}
		if p.Level == project.LevelSealed || !p.Level.OrDefault().Admits(candidate.Level.OrDefault()) {
			reasons = append(reasons, candidate.Node+": 数据等级不允许")
			continue
		}
		selected := configured
		selected.Node = candidate.Node
		candidate.Agent = selected
		return selected, candidate, nil
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "没有其他已注册且在线的同类Agent节点")
	}
	return agent.Agent{}, roster.Candidate{}, errors.New(strings.Join(reasons, "；"))
}

// relocationRequest is the approved plan and the attempt it replaces, once
// the approval is this requester's, this conversation's, and the task
// still authorizes the original execution.
func (c *Coordinator) relocationRequest(ctx context.Context, planID string, req Request) (attempt.RelocationIntent, attempt.Record, error) {
	p, err := c.attempts.Relocation(ctx, planID)
	if err != nil {
		return attempt.RelocationIntent{}, attempt.Record{}, err
	}
	old, err := c.attempts.Get(ctx, p.SourceID)
	if err != nil {
		return attempt.RelocationIntent{}, attempt.Record{}, err
	}
	if p.Owner != req.SenderOpenID || req.Relocation == nil || p.InputDigest != relocationRequestDigest(req) {
		return attempt.RelocationIntent{}, attempt.Record{}, errors.New("relocation approval does not match the original request context")
	}
	tracked, ok := c.tasks.Get(old.TaskID)
	if !ok || tracked.Channel != req.ConversationID || old.TurnID != req.MessageID || (req.ExpectedProject != "" && req.ExpectedProject != old.Project) {
		return attempt.RelocationIntent{}, attempt.Record{}, errors.New("relocation does not belong to this task conversation")
	}
	c.rememberMode(req)
	if old.Execution == nil {
		return attempt.RelocationIntent{}, attempt.Record{}, errors.New("original task authorization is missing")
	}
	if err := c.tasks.CheckExecution(*old.Execution); err != nil {
		return attempt.RelocationIntent{}, attempt.Record{}, err
	}
	if err := c.require(ctx, old.Project, req.SenderOpenID, project.RoleWrite); err != nil {
		return attempt.RelocationIntent{}, attempt.Record{}, err
	}
	return p, old, nil
}

// relocationPreparation is what an earlier delivery of the same approval
// left: a replacement already running or done (resume it), one interrupted
// before its input (admitted), or nothing, in which case the source is
// inspected once more so a live execution is reattached, not replaced.
func (c *Coordinator) relocationPreparation(ctx context.Context, p attempt.RelocationIntent, old attempt.Record) (admitted *attempt.Record, proof *attempt.RetainedEvidence, resume bool, err error) {
	if existing, loadErr := c.attempts.Get(ctx, p.Target.ID); loadErr == nil {
		if existing.Recovery == nil || existing.Recovery.PlanID != p.ID {
			return nil, nil, false, errors.New("replacement attempt belongs to another plan")
		}
		if old.State == attempt.Superseded && !old.Unsettled && old.SupersededBy == existing.ID && c.executions != nil {
			c.executions.Resolve(old.ID)
		}
		if existing.State == attempt.Running || existing.State == attempt.Bound {
			return nil, nil, true, nil
		}
		if !attempt.PreparingRelocation(existing) {
			return nil, nil, false, retainedBlocked("relocation-preparation", "读取此前已确认方案的准备记录", "此前的新执行在发送输入前中断。", "需要核实已创建的原生会话与资源，不能凭重试覆盖它们。", "建议重新核对该准备记录并建立新的具体恢复方案。", nil)
		}
		admitted = &existing
	}
	if admitted == nil {
		// Without evidence the plan is approved on the record alone; the
		// operator sees why the original could not be looked at.
		var inspectErr error
		if proof, inspectErr = c.inspectRelocation(ctx, old); inspectErr != nil {
			slog.Warn(fmt.Sprintf("turn: inspect attempt %s before relocation: %v", old.ID, inspectErr), "plan", p.ID, "attempt", old.ID, "node", old.Node)
		}
	}
	if proof != nil && proof.Session.Command != nil && !proof.Session.ProcessStopped && (proof.Session.State == nodewire.SessionRunning || proof.Session.Command.Settled) {
		return nil, nil, false, errors.New("original execution is live or settled; reattach it instead of replacing it")
	}
	return admitted, proof, false, nil
}

func relocationApproval(p attempt.RelocationIntent, choice, actor string, proof *attempt.RetainedEvidence) attempt.RelocationApproval {
	approval := attempt.RelocationApproval{PlanID: p.ID, Actor: actor, ChoiceID: choice, Node: proof}
	if choice == "confirm-stopped-and-retry:"+p.ID {
		approval.StoppedConfirmed = true
		approval.EffectsReviewed = true
		for _, action := range p.UnknownActions {
			approval.ActionResults = append(approval.ActionResults, checkpoint.ActionResolution{ActionID: action.ID, Outcome: checkpoint.ActionRetryAuthorized, Evidence: choice})
		}
	}
	return approval
}

// verifyRelocationTarget is the target agent and its candidate, once the
// agent is still what the plan was made for and the checkpoint's bytes are
// still there to run on.
func (c *Coordinator) verifyRelocationTarget(ctx context.Context, p attempt.RelocationIntent, old attempt.Record) (agent.Agent, roster.Candidate, error) {
	selected, candidate, err := c.relocationTarget(ctx, old, p.Target.Node)
	if err != nil {
		return agent.Agent{}, roster.Candidate{}, err
	}
	if relocationDigest(selected) != p.TargetConfigHash {
		return agent.Agent{}, roster.Candidate{}, errors.New("target Agent configuration changed; request a new plan")
	}
	manifest, ok, err := c.artifacts.Manifest(ctx, p.Checkpoint)
	if err != nil || !ok || manifest.Content == nil || !manifest.Content.Recoverable() {
		return agent.Agent{}, roster.Candidate{}, errors.New("complete copied checkpoint is no longer available")
	}
	repo, err := c.artifacts.Repo(ctx, p.Target.Project)
	if err != nil || !repo.Has(ctx, p.Checkpoint) {
		return agent.Agent{}, roster.Candidate{}, errors.New("checkpoint bytes could not be verified")
	}
	return selected, candidate, nil
}

// openRelocationAttempt is the replacement attempt: the one an interrupted
// delivery prepared, recovered, or a new one opened on the approval.
func (c *Coordinator) openRelocationAttempt(ctx context.Context, p attempt.RelocationIntent, old attempt.Record, admitted *attempt.Record, approval attempt.RelocationApproval) (attempt.Record, error) {
	if admitted != nil {
		return c.attempts.RecoverRelocationPreparation(ctx, admitted.ID)
	}
	r, err := c.attempts.OpenRelocation(ctx, p.ID, approval)
	if err != nil {
		return attempt.Record{}, err
	}
	// The source's exact leases and stop decision committed together.
	// Its ended observer is now resolved; keeping it quarantined would
	// make a later task stop report an already retired execution.
	if c.executions != nil {
		c.executions.Resolve(old.ID)
	}
	return r, nil
}

// bindRelocation prepares the replacement and binds its workspace to the
// task's recovery, carrying the source's spend forward.
func (c *Coordinator) bindRelocation(ctx context.Context, r, old attempt.Record, p attempt.RelocationIntent, admission ability.Admission) (attempt.Record, error) {
	if r.State == attempt.Leased {
		prepared, err := c.attempts.Advance(ctx, r.ID, attempt.Prepared, "relocation", func(next *attempt.Record) { next.Admission = &admission })
		if err != nil {
			return r, err
		}
		r = prepared
	}
	var priorUsage task.RecoveryUsage
	if old.Usage != nil {
		u := old.Usage
		priorUsage = task.RecoveryUsage{Tokens: task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output}, Model: u.Model, Reported: u.Reported}
	}
	if err := c.tasks.BindRecoveryWorkspace(*r.Execution, task.RecoveryWorkspace{ID: r.Workspace.ID, ProjectID: r.Project, NodeID: r.Node, Path: r.Workspace.Path, Base: r.Base, AgentID: r.Agent, HarnessID: r.Harness, AttemptID: r.ID, PlanID: p.ID, TurnID: r.TurnID}, priorUsage); err != nil {
		return r, err
	}
	return r, nil
}

// freezeRelocationSession assembles the replacement's session once and
// keeps it with the attempt. Every replacement gets a fresh bearer; only
// this attempt's exact persisted payload may reuse it after an
// interrupted open.
func (c *Coordinator) freezeRelocationSession(ctx context.Context, req Request, selected agent.Agent, bindings []ability.Binding, r attempt.Record) (attempt.RelocationSessionConfig, error) {
	extras, agentToken, err := c.gateExtras(ctx, req.ConversationID, selected, state.Session{})
	if err != nil {
		return attempt.RelocationSessionConfig{}, err
	}
	capabilities, err := c.assemble(selected, req, extras)
	if err != nil {
		return attempt.RelocationSessionConfig{}, err
	}
	frozen := attempt.RelocationSessionConfig{MCPServers: append(append([]acp.MCPServer(nil), capabilities.MCPServers...), roster.ToMCP(bindings)...), AgentToken: agentToken, Fingerprint: capabilities.Fingerprint, SessionConfigHash: capabilities.SessionFingerprint, Instructions: capabilities.Instructions}
	if err := c.attempts.RecordRelocationSession(ctx, r.ID, frozen); err != nil {
		return attempt.RelocationSessionConfig{}, err
	}
	return frozen, nil
}

// relocationSessionState is the conversation's record of the replacement
// session, saved tainted before it opens.
func (c *Coordinator) relocationSessionState(ctx context.Context, req Request, r attempt.Record, frozen attempt.RelocationSessionConfig) (state.Session, error) {
	binding, err := c.bindingFor(ctx, req)
	if err != nil {
		return state.Session{}, err
	}
	if saved := c.store.Conversation(req.ConversationID).Sessions[r.Agent]; saved.UpstreamID != "" && saved.UpstreamID != r.Session {
		if err := c.store.ArchiveSession(req.ConversationID, r.Agent, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return state.Session{}, err
		}
	}
	session := state.Session{ConversationID: req.ConversationID, AgentID: r.Agent, HarnessID: r.Harness, NodeID: r.Node, UpstreamID: r.Session, Workspace: r.Workspace.Path, ProjectID: r.Project, ProjectVersion: binding.Version, CapabilityHash: frozen.Fingerprint, SessionConfigHash: frozen.SessionConfigHash, AgentToken: frozen.AgentToken, Tainted: true}
	if err := c.store.SaveSession(session); err != nil {
		return state.Session{}, err
	}
	return session, nil
}

// openRelocation opens the replacement's node-owned session, records its
// identity, sets the source's model and options on it, and takes the
// attempt to running. known says a session exists on the node, whatever
// happened after.
func (c *Coordinator) openRelocation(ctx context.Context, req Request, r attempt.Record, selected agent.Agent, frozen attempt.RelocationSessionConfig, session state.Session) (harness.Runner, attempt.Record, state.Session, bool, error) {
	runner, err := c.runtime.OpenSession(ctx, placement(selected), r.Session, r.Workspace.Path, frozen.MCPServers)
	if err != nil {
		return nil, r, session, false, err
	}
	if !strings.HasPrefix(runner.ID(), "ns_") {
		return nil, r, session, false, errors.New("replacement requires a node-owned session")
	}
	r.Session = runner.ID()
	r, err = c.attempts.RecordSession(ctx, r.ID, "relocation", runner.ID())
	if err != nil {
		return nil, r, session, true, err
	}
	session.UpstreamID = runner.ID()
	if err := c.store.SaveSession(session); err != nil {
		return nil, r, session, true, err
	}
	if err := applyRecoveryPreferences(ctx, runner, r.Preferences); err != nil {
		return nil, r, session, true, retainedBlocked("relocation-options", "在目标原生会话设置并读回原执行的模型和选项", "目标会话不能按原设置继续任务。", err.Error(), "建议补齐目标Agent支持的模型和选项后重新检查；尚未向它发送原任务。", err)
	}
	r, err = c.attempts.Advance(ctx, r.ID, attempt.Running, "relocation", func(next *attempt.Record) { next.Session = runner.ID() })
	if err != nil {
		return nil, r, session, true, err
	}
	if err := c.bindExecutionGate(ctx, req.ConversationID, r.ID); err != nil {
		return nil, r, session, true, err
	}
	return runner, r, session, true, nil
}
