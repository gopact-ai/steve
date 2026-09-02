package artifact

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// localNode plays a node on this machine: commands run in a shell, blobs
// land in its own state directory. It exercises exactly the scripts a real
// node receives.
type localNode struct {
	root, state string
}

func (n *localNode) Exec(ctx context.Context, node, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, out)
	}
	return string(out), nil
}

func (n *localNode) PutBlob(_ context.Context, node, name string, content io.Reader, size int64) error {
	if err := os.MkdirAll(filepath.Join(n.state, "blobs"), 0o700); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(n.state, "blobs", name))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.CopyN(f, content, size)
	return err
}

func (n *localNode) GetBlob(_ context.Context, node, name string, into io.Writer) error {
	f, err := os.Open(filepath.Join(n.state, "blobs", name))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(into, f)
	return err
}

func (n *localNode) Git(context.Context, string) (string, string, string, error) {
	return "2.x", n.root, n.state, nil
}

func newStore(t *testing.T, node *localNode, home project.Home) (*Store, project.Project) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	p := project.Project{ID: "p", Home: home}
	if err := projects.Declare(context.Background(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = projects.Get(context.Background(), "p")
	return New(filepath.Join(t.TempDir(), "artifacts"), book, projects, node), p
}

func TestIsolatedWorkspacesOnTheHubFromBaseWithInputsAndPublish(t *testing.T) {
	ctx := context.Background()
	canonical := t.TempDir()
	write(t, canonical, "README", "hello")
	store, p := newStore(t, &localNode{}, project.Home{Path: canonical})

	// Two parallel steps from the same base, then a merge step that sees
	// both results under inputs/.
	a, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Owner: "att-b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Path == b.Path || a.Kind != project.KindWorktree || read(t, a.Path, "README") != "hello" {
		t.Fatalf("worktrees = %+v / %+v", a, b)
	}
	base := store.canonicalRef(ctx, "p")
	if base == "" {
		t.Fatal("the base snapshot was not named as the project's canonical")
	}
	write(t, a.Path, "a.go", "package a")
	write(t, b.Path, "b.go", "package b")
	ra, changed, err := store.Publish(ctx, a, base, "att-a", "step a")
	if err != nil || !changed || ra.Parent != base || len(ra.Receipts) != 1 || !ra.Durable(p) {
		t.Fatalf("publish a = %+v changed=%v err=%v", ra, changed, err)
	}
	rb, _, err := store.Publish(ctx, b, base, "att-b", "step b")
	if err != nil {
		t.Fatal(err)
	}
	// Names bind under CAS.
	if _, err := store.Bind(ctx, "steve/1/a", 0, ra.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(ctx, "steve/1/a", 0, rb.ID); err == nil {
		t.Fatal("a second bind at version 0 won")
	}
	m, err := store.Materialize(ctx, project.Request{Project: "p", Isolated: true, Base: base, Owner: "att-m",
		Inputs: []project.Input{{Name: "a", Artifact: ra.ID}, {Name: "b", Artifact: rb.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, m.Path, "inputs/a/a.go") != "package a" || read(t, m.Path, "inputs/b/b.go") != "package b" || read(t, m.Path, "a.go") != "<missing>" {
		t.Fatal("inputs were not materialised beside the base")
	}
	// The merge step's result excludes inputs/.
	write(t, m.Path, "a.go", "package a")
	write(t, m.Path, "b.go", "package b")
	rm, _, err := store.Publish(ctx, m, base, "att-m", "merge")
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := store.Repo(ctx, "p")
	paths, _ := repo.Changed(ctx, base, rm.ID)
	if strings.Join(paths, ",") != "a.go,b.go" {
		t.Fatalf("merge result paths = %v", paths)
	}
	if err := store.Discard(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Path); !os.IsNotExist(err) {
		t.Fatal("worktree survived discard")
	}
	// The user's directory was never touched.
	if entries, _ := os.ReadDir(canonical); len(entries) != 1 {
		t.Fatalf("canonical changed: %v", entries)
	}
}

func TestWorkspacesOnANodeTravelAsBundles(t *testing.T) {
	ctx := context.Background()
	node := &localNode{root: t.TempDir(), state: t.TempDir()}
	canonical := filepath.Join(node.root, "proj")
	write(t, canonical, "f", "0")
	// The project lives on the node; the hub has nothing of it yet.
	store, _ := newStore(t, node, project.Home{Node: "node-a", Path: canonical})

	ws, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Owner: "att-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ws.Path, filepath.Join(node.root, "worktrees")) || read(t, ws.Path, "f") != "0" {
		t.Fatalf("node worktree = %+v", ws)
	}
	base := store.canonicalRef(ctx, "p")
	hub, _ := store.Repo(ctx, "p")
	if !hub.Has(ctx, base) {
		t.Fatal("the canonical snapshot taken on the node did not reach the hub")
	}
	write(t, ws.Path, "f", "1")
	write(t, ws.Path, "g", "new")
	result, changed, err := store.Publish(ctx, ws, base, "att-1", "step")
	if err != nil || !changed {
		t.Fatalf("publish = %+v changed=%v err=%v", result, changed, err)
	}
	if !hub.Has(ctx, result.ID) || len(result.Receipts) != 1 {
		t.Fatal("the result did not reach the hub durably")
	}
	// A second workspace on the node for a hub-side artifact: pushed, not
	// re-snapshotted.
	other, err := store.Materialize(ctx, project.Request{Project: "p", Node: "node-a", Isolated: true, Base: result.ID, Owner: "att-2"})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, other.Path, "g") != "new" {
		t.Fatal("the node worktree does not match the artifact")
	}
	// Unchanged publish returns the parent, no new artifact.
	same, changed, err := store.Publish(ctx, other, result.ID, "att-2", "nothing")
	if err != nil || changed || same.ID != result.ID {
		t.Fatalf("unchanged publish = %+v changed=%v err=%v", same, changed, err)
	}
}
