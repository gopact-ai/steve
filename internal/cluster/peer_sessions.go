package cluster

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/plugins/pluginledger"
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
	_, _, err := sessionActionMode(action)
	if err != nil {
		return err
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
	return authorizeSessionRead(ctx, runtime.Ledger(), binding, action, func(record attempt.Record) error {
		if record.PluginRuntime == nil {
			return nil
		}
		declared, found, err := platformconfig.New(runtime.Ledger()).Load()
		if err != nil {
			return err
		}
		if !found {
			return coordination.ErrNotReady
		}
		return (&pluginledger.Library{Ledger: runtime.Ledger()}).CheckRuntimeScope(ctx, declared.Plugins, *record.PluginRuntime)
	})
}
