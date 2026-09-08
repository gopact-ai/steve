package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

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
			Binding:   nodewire.SessionBinding{ProjectID: record.Project, SessionID: cluster.LogicalAgentSession(tracked.Channel, tracked.ID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(record), TaskEpoch: record.Execution.Epoch},
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
