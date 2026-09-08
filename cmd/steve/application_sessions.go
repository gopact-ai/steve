package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func logicalAgentSession(channel, taskID, agentID string) string {
	return attempt.RetainedSessionID(channel, taskID, agentID)
}

func validateSessionPlacement(record attempt.Record, place harness.Placement, upstream, workdir string) error {
	if record.Node != place.Node || record.Harness != place.Harness {
		return errors.New("node session does not match its admitted execution")
	}
	if workdir == "" {
		// Capability discovery carries the admitted attempt but has no native
		// session yet. The node validates its distinct capabilities action;
		// creating a native session still requires a nonempty workdir there.
		if upstream == "" {
			return nil
		}
		if upstream != record.Session || !strings.HasPrefix(upstream, "ns_") {
			return errors.New("session cleanup requires its committed native identity")
		}
		return nil
	}
	if filepath.Clean(record.Workspace.Path) != filepath.Clean(workdir) {
		return errors.New("node session workspace differs from its admitted execution")
	}
	return nil
}

func newApplicationSessionBinder(active cluster.Activation) func(context.Context, harness.Placement, string, string) (context.Context, error) {
	return func(ctx context.Context, place harness.Placement, upstream string, workdir string) (context.Context, error) {
		key, ok := execution.KeyOf(ctx)
		if !ok || key.AttemptID == "" {
			if workdir != "" || !strings.HasPrefix(upstream, "ns_") {
				return ctx, nil
			}
			// /new runs between turns, without an execution scope. It may
			// close only the exact persisted native session selected by the
			// conversation, never synthesize a fresh execution binding.
			latest, err := sessionCleanupRecord(ctx, active, place, upstream)
			if err != nil {
				return nil, err
			}
			key = execution.Key{TaskID: latest.TaskID, AttemptID: latest.ID}
		}
		if err := active.Context.Err(); err != nil {
			return nil, err
		}
		record, err := attempt.New(active.Ledger).Get(ctx, key.AttemptID)
		if err != nil {
			return nil, err
		}
		if record.TaskID != key.TaskID {
			return nil, errors.New("node session does not match its admitted execution")
		}
		if err := validateSessionPlacement(record, place, upstream, workdir); err != nil {
			return nil, err
		}
		if record.Execution == nil || record.Execution.TaskID != record.TaskID {
			return nil, errors.New("node session requires a task execution token")
		}
		tasks, err := task.OpenLedger(active.Ledger, "")
		if err != nil {
			return nil, err
		}
		tracked, ok := tasks.Get(record.TaskID)
		if !ok {
			return nil, errors.New("node session task is missing")
		}
		command := attempt.InputCommandID(record)
		return harness.WithNodeSession(ctx, harness.NodeSessionContext{
			Authority: nodewire.SessionAuthority{ClusterID: active.Runtime.Status().ClusterID, CoordinatorNodeID: active.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration},
			Binding:   nodewire.SessionBinding{ProjectID: record.Project, SessionID: logicalAgentSession(tracked.Channel, tracked.ID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(record), TaskEpoch: record.Execution.Epoch},
			CommandID: command,
		}), nil
	}
}

func sessionCleanupRecord(ctx context.Context, active cluster.Activation, place harness.Placement, upstream string) (attempt.Record, error) {
	if err := active.Context.Err(); err != nil {
		return attempt.Record{}, err
	}
	service := attempt.New(active.Ledger)
	live, err := service.Live(ctx)
	if err != nil {
		return attempt.Record{}, err
	}
	closed, err := service.Closed(ctx)
	if err != nil {
		return attempt.Record{}, err
	}
	var latest attempt.Record
	var latestSequence int64
	for _, record := range append(live, closed...) {
		if record.Session != upstream || record.Node != place.Node || record.Harness != place.Harness {
			continue
		}
		// Event sequence is shared ledger order. Wall clocks on successive
		// coordinators cannot identify the newest native-session binding.
		events, err := active.Ledger.Events(ctx, record.ID)
		if err != nil {
			return attempt.Record{}, err
		}
		if len(events) == 0 || events[0].From != "" {
			return attempt.Record{}, errors.New("native session lacks its committed creation order")
		}
		sequence := events[0].Seq
		if latest.ID == "" || sequence > latestSequence {
			latest = record
			latestSequence = sequence
			continue
		}
		if latest.ID != record.ID && sequence == latestSequence {
			return attempt.Record{}, errors.New("native session has ambiguous execution history")
		}
	}
	if latest.ID == "" {
		return attempt.Record{}, errors.New("native session has no committed execution history")
	}
	return latest, nil
}

func (p *clusterPeer) AuthorizeNodeSession(ctx context.Context, authenticatedNode string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, action nodewire.SessionAction) error {
	if authenticatedNode != authority.CoordinatorNodeID {
		return errors.New("node session coordinator differs from the authenticated peer")
	}
	return p.authorizeSessionExecution(ctx, p.config.NodeID, authority, binding, action)
}

func (p *clusterPeer) applicationSessionAuthorizer(active cluster.Activation) func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error {
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

func (p *clusterPeer) authorizeSessionExecution(ctx context.Context, node string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, action nodewire.SessionAction) error {
	if authority.ClusterID != p.config.ClusterID || binding.NodeID != node {
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
	runtime := p.runtime.Load()
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
	for {
		version, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return err
		}
		if version >= state.AppVersion {
			break
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	record, err := attempt.New(runtime.Ledger()).Get(ctx, binding.AttemptID)
	if err != nil {
		return err
	}
	if record.TaskID != binding.TaskID || record.Project != binding.ProjectID || record.Node != binding.NodeID || attempt.SessionExecutionEpoch(record) != binding.ExecutionEpoch || record.Execution == nil || record.Execution.Epoch != binding.TaskEpoch {
		return errors.New("node session differs from the committed execution")
	}
	tasks, err := task.OpenLedger(runtime.Ledger(), "")
	if err != nil {
		return err
	}
	tracked, ok := tasks.Get(record.TaskID)
	if !ok || binding.SessionID != logicalAgentSession(tracked.Channel, tracked.ID, record.Agent) {
		return errors.New("node session conversation differs")
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
