package mesh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/project"
)

// C11: an artifact made on node-b reaches node-a without passing through
// the hub: node-b bundles it, the hub grants node-a one fetch, node-a
// pulls it over the 1 ms link between them. The replica record says so,
// and node-b's log shows the peer.
func TestC11ArtifactsMoveNodeToNodeUnderTheHubsGrant(t *testing.T) {
	requireMesh(t)
	f := newFleet(t)
	f.artifacts.Direct = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A result made on node-b: published, so the hub and node-b hold it.
	ws, err := f.artifacts.Materialize(ctx, project.Request{Project: "local", Node: nodeB, Isolated: true, Owner: "att-b"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := onNode(t, nodeB, "printf made-on-b > "+ws.Path+"/direct.txt"); err != nil {
		t.Fatalf("write on node-b: %v\n%s", err, out)
	}
	result, changed, err := f.artifacts.Publish(ctx, ws, ws.Base, "att-b", "made on node-b")
	if err != nil || !changed {
		t.Fatalf("publish = %+v changed=%v err=%v", result, changed, err)
	}
	_ = f.artifacts.Discard(ctx, ws)

	// Materialising it on node-a takes it from node-b, not the hub.
	before, _ := onNode(t, nodeB, "grep -c 'peer' ~/steve-node.log || true")
	other, err := f.artifacts.Materialize(ctx, project.Request{Project: "local", Node: nodeA, Isolated: true, Base: result.ID, Owner: "att-a"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.artifacts.Discard(context.Background(), other) })
	if out, err := onNode(t, nodeA, "cat "+other.Path+"/direct.txt"); err != nil || strings.TrimSpace(out) != "made-on-b" {
		t.Fatalf("node-a worktree content = %q err=%v", out, err)
	}
	replicas, _ := f.artifacts.Replicas(ctx, result.ID)
	var onA string
	for _, r := range replicas {
		if r.Node == nodeA {
			onA = r.State + " " + r.Note
		}
	}
	if !strings.HasPrefix(onA, "verified direct from "+nodeB) {
		t.Fatalf("replica on node-a = %q, want a verified direct transfer from node-b (replicas: %+v)", onA, replicas)
	}
	after, _ := onNode(t, nodeB, "grep -c 'peer' ~/steve-node.log || true")
	if strings.TrimSpace(after) == strings.TrimSpace(before) {
		t.Fatalf("node-b saw no peer connection (before=%s after=%s)", strings.TrimSpace(before), strings.TrimSpace(after))
	}
	t.Logf("node-b peer log lines: %s -> %s", strings.TrimSpace(before), strings.TrimSpace(after))
}
