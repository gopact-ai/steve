package mesh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/project"
)

// C9: a sealed project homed on node-b, whose git predates merge-tree
// --write-tree. Its objects never reach the hub; a worktree is cut on
// node-b, a change published there, and the landing merges in place on
// node-b through the legacy path — against an edit the user made to the
// canonical directory in between.
func TestC9SealedProjectLivesAndLandsOnItsOldGitNode(t *testing.T) {
	requireMesh(t)
	host := addrHost(addrB())
	canonical := nodeWork + "/vault-b"
	if out, err := sshOut(t, host, "rm -rf "+canonical+" && mkdir -p "+canonical+" && printf f0 > "+canonical+"/f && git --version"); err != nil {
		t.Fatalf("prepare node-b: %v\n%s", err, out)
	} else {
		t.Logf("node-b %s", strings.TrimSpace(out))
	}
	t.Cleanup(func() { _, _ = sshOut(t, host, "rm -rf "+canonical) })

	reg := node.NewRegistry("hub-e2e", map[string]node.Config{
		nodeA: {Addr: addrA(), Token: tokenA(), DialTimeout: 10 * time.Second},
		nodeB: {Addr: addrB(), Token: tokenB(), DialTimeout: 10 * time.Second, Level: "sealed"},
	})
	t.Cleanup(reg.Close)
	reg.SetHubLevel("restricted")
	reg.EnsureConnected(t.Context())
	dir := t.TempDir()
	_, _, artifacts := declareProjects(t, dir, reg,
		project.Project{ID: "vault-b", Level: project.LevelSealed, Home: project.Home{Node: nodeB, Path: canonical}},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, _, _ := artifacts.Project(ctx, "vault-b")

	// Not on the hub, not on node-a: sealed runs only at home.
	if _, err := artifacts.Materialize(ctx, project.Request{Project: "vault-b", Node: "", Isolated: true, Owner: "att-hub"}); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("sealed project materialised on the hub: %v", err)
	}
	if _, err := artifacts.Materialize(ctx, project.Request{Project: "vault-b", Node: nodeA, Isolated: true, Owner: "att-a"}); err == nil {
		t.Fatal("sealed project materialised on node-a")
	}
	ws, err := artifacts.Materialize(ctx, project.Request{Project: "vault-b", Node: nodeB, Isolated: true, Owner: "att-b"})
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := artifacts.Repo(ctx, "vault-b")
	if hub.Has(ctx, ws.Base) {
		t.Fatal("sealed objects reached the hub's repository")
	}
	base, _, _ := artifacts.Manifest(ctx, ws.Base)
	if len(base.Receipts) != 1 || base.Receipts[0].Place != nodeB || !base.Durable(p) {
		t.Fatalf("sealed base manifest = %+v", base)
	}
	// The step (played by ssh) writes in its worktree; the user edits f in
	// place meanwhile.
	if out, err := sshOut(t, host, "printf g1 > "+ws.Path+"/g"); err != nil {
		t.Fatalf("write in worktree: %v\n%s", err, out)
	}
	result, changed, err := artifacts.Publish(ctx, ws, ws.Base, "att-b", "step on node-b")
	if err != nil || !changed || hub.Has(ctx, result.ID) {
		t.Fatalf("publish = %+v changed=%v err=%v hubHas=%v", result, changed, err, hub.Has(ctx, result.ID))
	}
	if out, err := sshOut(t, host, "printf f-user > "+canonical+"/f"); err != nil {
		t.Fatalf("user edit: %v\n%s", err, out)
	}
	land, err := artifacts.Land(ctx, p, result.ID, "e2e")
	if err != nil || land.State != "committed" {
		t.Fatalf("land = %+v err=%v", land, err)
	}
	out, err := sshOut(t, host, "cat "+canonical+"/f; echo; cat "+canonical+"/g; echo; ls "+nodeWork+"/worktrees | wc -l")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || lines[0] != "f-user" || lines[1] != "g1" {
		t.Fatalf("node-b canonical after landing: %q", out)
	}
	if err := artifacts.Discard(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if out, _ := sshOut(t, host, "ls "+nodeWork+"/worktrees | wc -l"); strings.TrimSpace(out) != "0" {
		t.Fatalf("worktrees left on node-b: %s", strings.TrimSpace(out))
	}
	// The hub holds metadata only: manifests and replicas, no objects.
	replicas, _ := artifacts.Replicas(ctx, result.ID)
	if len(replicas) != 1 || replicas[0].Node != nodeB || replicas[0].State != "verified" {
		t.Fatalf("replicas of the sealed result = %+v", replicas)
	}
}
