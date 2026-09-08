package artifact

import (
	"context"
	"os"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/project"
)

// countingNode counts the operations a store sends to a node: on a distant
// machine every one of them is a round trip.
type countingNode struct {
	*localNode
	ops map[ops.Kind]int
}

func (n *countingNode) Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	n.ops[req.Op]++
	return n.localNode.Artifact(ctx, node, req)
}

func TestRemoteSnapshotAsksOnceAboutWhatItAlreadyKnows(t *testing.T) {
	ctx := context.Background()
	inner := &localNode{root: t.TempDir(), state: t.TempDir()}
	node := &countingNode{localNode: inner, ops: map[ops.Kind]int{}}
	work := t.TempDir()
	write(t, work, "a", "one")
	store, p := newStore(t, inner, project.Home{Node: "node", Path: work})
	store.nodes = node
	ws := project.Workspace{ID: "copy", Project: p.ID, Node: "node", Path: work, Kind: project.KindCopy}
	first, changed, err := store.SnapshotWorkspace(ctx, p, ws, "", "test", "first")
	if err != nil || !changed {
		t.Fatalf("first snapshot: changed=%v err=%v", changed, err)
	}
	if node.ops[ops.Init] != 1 || node.ops[ops.Snapshot] != 1 {
		t.Fatalf("first snapshot ops = %v", node.ops)
	}
	// Nothing changed: the shadow repository and the parent are known to
	// be there, so the node is asked for the snapshot and nothing else.
	again, changed, err := store.SnapshotWorkspace(ctx, p, ws, first.ID, "test", "again")
	if err != nil || changed || again.ID != first.ID {
		t.Fatalf("unchanged snapshot: %+v changed=%v err=%v", again, changed, err)
	}
	if node.ops[ops.Init] != 1 || node.ops[ops.Has] != 0 || node.ops[ops.Snapshot] != 2 {
		t.Fatalf("unchanged snapshot ops = %v", node.ops)
	}
	// The node comes back as another generation: the shadow repository and
	// the replica are checked once more, then trusted again.
	inner.gen = 2
	if _, _, err := store.SnapshotWorkspace(ctx, p, ws, first.ID, "test", "new generation"); err != nil {
		t.Fatal(err)
	}
	if node.ops[ops.Init] != 2 || node.ops[ops.Has] != 1 {
		t.Fatalf("new generation ops = %v", node.ops)
	}
	if _, _, err := store.SnapshotWorkspace(ctx, p, ws, first.ID, "test", "trusted again"); err != nil {
		t.Fatal(err)
	}
	if node.ops[ops.Init] != 2 || node.ops[ops.Has] != 1 {
		t.Fatalf("trusted again ops = %v", node.ops)
	}
	// The node lost its shadow repository behind the hub's back: the
	// snapshot still succeeds, after the node is checked and set up again.
	if err := os.RemoveAll(nodeBare(inner.state, p.ID)); err != nil {
		t.Fatal(err)
	}
	write(t, work, "a", "two")
	second, changed, err := store.SnapshotWorkspace(ctx, p, ws, first.ID, "test", "after loss")
	if err != nil || !changed || second.ID == first.ID {
		t.Fatalf("snapshot after the node lost its repository: %+v changed=%v err=%v", second, changed, err)
	}
	if node.ops[ops.Init] != 3 || node.ops[ops.Has] < 2 || node.ops[ops.Unbundle] < 1 {
		t.Fatalf("recovery ops = %v", node.ops)
	}
	if r, ok := store.replica(ctx, first.ID, "node"); !ok || r.State != ReplicaVerified {
		t.Fatalf("parent replica after recovery = %+v ok=%v", r, ok)
	}
}
