package artifact

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/project"
)

type parentInspectionNodes struct {
	LocalNodes
	root string
	seen ops.Request
}

func (n *parentInspectionNodes) Git(context.Context, string) (string, string, string, error) {
	return "", n.root, "", nil
}

func (n *parentInspectionNodes) Artifact(_ context.Context, _ string, req ops.Request) (ops.Result, error) {
	n.seen = req
	return ops.Result{}, nil
}

func TestRecoveryCopyInspectionUsesTheNodeMetadataParent(t *testing.T) {
	for _, tc := range []struct{ root, work, parent string }{
		{`c:\root`, `c:\root\worktrees\container\work`, `c:\root\worktrees\container`},
		{`\\server\share\root`, `\\server\share\root\worktrees\container\work`, `\\server\share\root\worktrees\container`},
		{`/srv/root`, `/srv/root/worktrees/container/work`, `/srv/root/worktrees/container`},
	} {
		n := &parentInspectionNodes{root: tc.root}
		s := New(t.TempDir(), nil, nil, n)
		r := attempt.WorkspaceRecovery{ID: "inspection-fixture", Phase: "draining", Workspace: project.Workspace{Node: "remote", Path: tc.work}}
		r.Head.Version = 1
		if err := s.verifyRecoveryCopy(t.Context(), project.Project{}, r); err != nil {
			t.Fatal(err)
		}
		if n.seen.Op != ops.InspectRecovery || n.seen.Path != tc.parent || n.seen.WorkTree != tc.root {
			t.Errorf("inspection changed node syntax: %+v, want root=%q parent=%q", n.seen, tc.root, tc.parent)
		}
	}
}
