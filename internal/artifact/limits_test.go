package artifact

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/project"
)

func TestStorePreservesSnapshotTooLarge(t *testing.T) {
	for _, node := range []string{"", "node"} {
		t.Run("node="+node, func(t *testing.T) {
			n := &localNode{root: t.TempDir(), state: t.TempDir()}
			work := t.TempDir()
			store, p := newStore(t, n, project.Home{Node: node, Path: work})
			store.Limits = gitrepo.Limits{MaxFiles: 2}
			for _, name := range []string{"a", "b", "c"} {
				write(t, work, name, name)
			}
			for _, snapshot := range []func(context.Context) (Manifest, bool, error){
				func(ctx context.Context) (Manifest, bool, error) {
					return store.SnapshotCanonical(ctx, p, "", "test", "too large")
				},
				func(ctx context.Context) (Manifest, bool, error) {
					return store.SnapshotWorkspace(ctx, p, project.Workspace{ID: "copy", Project: p.ID, Node: node, Path: work, Kind: project.KindCopy}, "", "test", "too large")
				},
				func(ctx context.Context) (Manifest, bool, error) {
					return store.Publish(ctx, project.Workspace{Project: p.ID, Node: node, Path: work, Kind: project.KindWorktree}, "", "test", "too large")
				},
			} {
				_, changed, err := snapshot(t.Context())
				if err != (gitrepo.TooLarge{Which: "files", Have: 3, Limit: 2}) || changed {
					t.Fatalf("store swallowed or wrapped the limit: changed=%v, %v", changed, err)
				}
			}
			if head := store.CanonicalOf(t.Context(), p.ID); head != "" {
				t.Fatalf("failed snapshot moved the canonical head: %s", head)
			}
		})
	}
}
