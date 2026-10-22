package lifecycle

import (
	"context"
	"fmt"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
)

func (e *Execution) preparePluginRuntime(ctx context.Context) (*plugins.RuntimeRef, error) {
	preparer, ok := e.o.Sessions.(harness.PluginSessionPreparer)
	if !ok {
		if e.Record.PluginRuntime != nil {
			return nil, fmt.Errorf("plugin runtime preparation is unavailable")
		}
		return nil, nil
	}
	ref, err := preparer.PreparePluginSession(ctx, harness.PluginPreparation{AgentID: e.Record.Agent, At: e.o.At, Project: e.Record.Project, AttemptID: e.Record.ID, Upstream: e.Upstream, Prior: e.Record.PluginRuntime})
	if err != nil {
		return nil, err
	}
	if ref != nil {
		if err := ref.Validate(); err != nil {
			return nil, err
		}
		if ref.Selection.Project != e.Record.Project || ref.Selection.Node != e.o.At.Node || ref.Selection.Harness != e.o.At.Harness {
			return nil, fmt.Errorf("%w: prepared plugin runtime %s is for project %q on %q/%s, not this attempt's project %q on %q/%s", plugins.ErrInvalid, ref.ID, ref.Selection.Project, ref.Selection.Node, ref.Selection.Harness, e.Record.Project, e.o.At.Node, e.o.At.Harness)
		}
	}
	return ref, nil
}
