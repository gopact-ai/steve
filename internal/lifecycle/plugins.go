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
	if ref != nil && (ref.Validate() != nil || ref.Selection.Project != e.Record.Project || ref.Selection.Node != e.o.At.Node || ref.Selection.Harness != e.o.At.Harness) {
		return nil, plugins.ErrInvalid
	}
	return ref, nil
}
