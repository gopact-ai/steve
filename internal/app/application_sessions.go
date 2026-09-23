package app

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
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
		if upstream != record.Session || !nodewire.IsManagedSession(upstream) {
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
			if workdir != "" || !nodewire.IsManagedSession(upstream) {
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
		record, tracked, err := readSessionBinding(ctx, active.Ledger, key, place, upstream, workdir)
		if err != nil {
			return nil, err
		}
		command := attempt.InputCommandID(record)
		ctx = harness.WithPluginProfile(ctx, record.PluginRuntime)
		return harness.WithNodeSession(ctx, harness.NodeSessionContext{
			Authority:    nodewire.SessionAuthority{ClusterID: active.Runtime.Status().ClusterID, CoordinatorNodeID: active.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration},
			Binding:      sessionBinding(record, tracked),
			NativeImport: record.NativeImport.Clone(),
			CommandID:    command,
		}), nil
	}
}

func sessionCleanupRecord(ctx context.Context, active cluster.Activation, place harness.Placement, upstream string) (attempt.Record, error) {
	if err := active.Context.Err(); err != nil {
		return attempt.Record{}, err
	}
	latest, found, err := attempt.New(active.Ledger).LatestForSession(ctx, place.Node, place.Harness, upstream)
	if err != nil {
		return attempt.Record{}, err
	}
	if !found {
		return attempt.Record{}, errors.New("native session has no committed execution history")
	}
	return latest, nil
}
