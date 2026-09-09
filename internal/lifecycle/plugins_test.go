package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
)

type pluginSessions struct {
	*fakeSessions
	attempts *fakeAttempts
	ref      *plugins.RuntimeRef
	failure  error
	t        *testing.T
}

func (s *pluginSessions) PreparePluginSession(_ context.Context, req harness.PluginPreparation) (*plugins.RuntimeRef, error) {
	s.attempts.log("plugin-prepare")
	if s.attempts.record.State != attempt.Leased || req.AttemptID != "a1" || req.Project != "p" {
		s.t.Error("plugin preparation escaped leased attempt")
	}
	return s.ref, s.failure
}
func (s *pluginSessions) OpenSession(ctx context.Context, at harness.Placement, upstream, dir string, servers []acp.MCPServer) (harness.Runner, error) {
	ref := harness.PluginProfile(ctx)
	if ref == nil || s.attempts.record.PluginRuntime == nil || ref.ID != s.attempts.record.PluginRuntime.ID || s.attempts.record.State != attempt.Prepared {
		s.t.Error("native open happened before durable runtime binding")
	}
	return s.fakeSessions.OpenSession(ctx, at, upstream, dir, servers)
}

func TestRuntimeBindingCommitsBeforeNativeOpen(t *testing.T) {
	world := newWorld("s1")
	ref := &plugins.RuntimeRef{ID: strings.Repeat("a", 64), Selection: plugins.Selection{Project: "p", Node: "n1", Harness: "mock", Deployments: []string{strings.Repeat("b", 64)}}}
	sessions := &pluginSessions{fakeSessions: world.sessions, attempts: world.attempts, ref: ref, t: t}
	options := world.options()
	options.Sessions = sessions
	options.Spec.Project = "p"
	options.At = harness.Placement{Node: "n1", Harness: "mock"}
	result, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.PluginRuntime == nil || result.Record.PluginRuntime.ID != ref.ID {
		t.Fatal("attempt lost runtime identity")
	}
	history := world.attempts.history()
	if !strings.Contains(history, "admit plugin-prepare prepared") {
		t.Fatalf("wrong admission/preparation order: %s", history)
	}
}

func TestFailedRuntimePreparationClosesAttemptWithoutOpeningNativeSession(t *testing.T) {
	world := newWorld("s1")
	fault := errors.New("missing plugin credential")
	sessions := &pluginSessions{fakeSessions: world.sessions, attempts: world.attempts, failure: fault, t: t}
	options := world.options()
	options.Sessions = sessions
	options.Spec.Project = "p"
	options.At = harness.Placement{Node: "n1", Harness: "mock"}
	result, err := Run(t.Context(), options)
	if !errors.Is(err, fault) || result.Record.State != attempt.Failed || world.sessions.opened != 0 || world.workspaces.discarded != 1 {
		t.Fatalf("failed runtime preparation: %+v %v", result, err)
	}
}
