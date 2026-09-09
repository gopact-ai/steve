package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/task"
)

func LogicalAgentSession(channel, taskID, agentID string) string {
	return attempt.RetainedSessionID(channel, taskID, agentID)
}

func (p *Peer) AuthorizeNodeSession(ctx context.Context, authenticatedNode string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, action nodewire.SessionAction) error {
	if authenticatedNode != authority.CoordinatorNodeID {
		return errors.New("node session coordinator differs from the authenticated peer")
	}
	return p.authorizeSessionExecution(ctx, p.Config.NodeID, authority, binding, action)
}

func (p *Peer) ApplicationSessionAuthorizer(active Activation) func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error {
	return func(ctx context.Context, node string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, action nodewire.SessionAction) error {
		if err := active.Context.Err(); err != nil {
			return err
		}
		if authority.CoordinatorNodeID != active.NodeID || authority.CoordinatorEpoch != active.Assignment.Epoch || authority.WriterGeneration != active.WriterGeneration {
			return coordination.ErrStaleEpoch
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(active.Context, cancel)
		defer stop()
		if err := p.authorizeSessionExecution(ctx, node, authority, binding, action); err != nil {
			return err
		}
		return active.Context.Err()
	}
}

func (p *Peer) authorizeSessionExecution(ctx context.Context, node string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, action nodewire.SessionAction) error {
	if authority.ClusterID != p.Config.ClusterID || binding.NodeID != node {
		return errors.New("node session belongs to another cluster or machine")
	}
	var observation, stopping bool
	switch action {
	case nodewire.SessionActionOpen, nodewire.SessionActionAttach, nodewire.SessionActionPoll, nodewire.SessionActionSettings, nodewire.SessionActionInspectOpen:
		observation = true
	case nodewire.SessionActionCancel, nodewire.SessionActionAbort, nodewire.SessionActionClose, nodewire.SessionActionCancelOpen:
		stopping = true
	case nodewire.SessionActionStart, nodewire.SessionActionPrompt, nodewire.SessionActionAnswer, nodewire.SessionActionOption, nodewire.SessionActionCapabilities:
	default:
		return errors.New("unsupported node session action")
	}
	runtime := p.Runtime.Load()
	if runtime == nil {
		return coordination.ErrUnavailable
	}
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return err
	}
	if state.Coordinator.NodeID != authority.CoordinatorNodeID || state.Coordinator.Epoch != authority.CoordinatorEpoch || state.WriterGeneration != authority.WriterGeneration {
		return coordination.ErrStaleEpoch
	}
	var poll *time.Ticker
	for {
		version, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return err
		}
		if version >= state.AppVersion {
			break
		}
		if poll == nil {
			poll = time.NewTicker(5 * time.Millisecond)
			defer poll.Stop()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
	record, err := attempt.New(runtime.Ledger()).Get(ctx, binding.AttemptID)
	if err != nil {
		return err
	}
	if record.PluginRuntimeID() != binding.PluginRuntimeID || record.TaskID != binding.TaskID || record.Project != binding.ProjectID || record.Node != binding.NodeID || attempt.SessionExecutionEpoch(record) != binding.ExecutionEpoch || record.Execution == nil || record.Execution.Epoch != binding.TaskEpoch {
		return errors.New("node session differs from the committed execution")
	}
	tasks, err := task.OpenLedger(runtime.Ledger(), "")
	if err != nil {
		return err
	}
	tracked, ok := tasks.Get(record.TaskID)
	if !ok || binding.SessionID != LogicalAgentSession(tracked.Channel, tracked.ID, record.Agent) {
		return errors.New("node session conversation differs")
	}
	if !stopping && record.PluginRuntime != nil {
		declared, found, err := platformconfig.New(runtime.Ledger()).Load()
		if err != nil {
			return err
		}
		if !found {
			return coordination.ErrNotReady
		}
		if err := (&plugins.Library{Ledger: runtime.Ledger()}).CheckRuntimeScope(ctx, declared.Plugins, *record.PluginRuntime); err != nil {
			return err
		}
	}
	if !stopping && !observation {
		if record.State.Terminal() || record.Unsettled {
			return fmt.Errorf("execution %s cannot start more work", record.ID)
		}
		if action == nodewire.SessionActionPrompt && record.State != attempt.Running {
			return errors.New("native input requires a committed running attempt")
		}
		if action == nodewire.SessionActionStart && record.State != attempt.Leased && record.State != attempt.Prepared && record.State != attempt.Running {
			return errors.New("native session creation is outside the execution preparation phase")
		}
		if err := tasks.CheckExecution(*record.Execution); err != nil {
			return err
		}
		for _, granted := range record.Leases {
			current, exists, err := runtime.Ledger().LeaseOf(ctx, granted.Key)
			if err != nil {
				return err
			}
			if !exists || current.Incarnation != granted.Incarnation || current.Epoch != granted.Epoch || current.Holder != granted.Holder || !current.ExpiresAt.After(time.Now()) {
				return ledger.ErrStale
			}
		}
	}
	return nil
}
