package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// localNode runs the same typed operations as a node, with local blobs.
type localNode struct {
	root, state string
	level       string
	gen         int64
}

func (n *localNode) Generation(context.Context, string) (int64, error) {
	if n.gen == 0 {
		return 1, nil
	}
	return n.gen, nil
}

func (n *localNode) Artifact(ctx context.Context, _ string, req ops.Request) (ops.Result, error) {
	return gitrepo.RunOperation(ctx, req)
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
	out, _ := exec.Command("git", "--version").Output()
	return string(out), n.root, n.state, nil
}

func (n *localNode) Region(context.Context, string) (string, error) { return "", nil }

func (n *localNode) Level(_ context.Context, node string) (string, error) {
	if node != "" && n.level != "" {
		return n.level, nil
	}
	return "internal", nil
}

func newStore(t *testing.T, node *localNode, home project.Home) (*Store, project.Project) {
	t.Helper()
	return newStoreWith(t, node, home, ledger.Options{})
}

// newStoreWith opens the store on a ledger tuned by opts, so a test can
// run leases against a clock it moves itself.
func newStoreWith(t *testing.T, node *localNode, home project.Home, opts ledger.Options) (*Store, project.Project) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), opts)
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
	base := canonicalOf(t, store, "p")
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
	base := canonicalOf(t, store, "p")
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

// canonicalOf reads the project's canonical name, failing the test when it
// cannot be read.
func canonicalOf(t *testing.T, store *Store, projectID string) string {
	t.Helper()
	head, err := store.CanonicalOf(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestRecoveryPreparationUsesTypedNodeOperationsAndRespectsDataPlacement(t *testing.T) {
	for _, level := range []datalevel.Level{datalevel.Internal, datalevel.Sealed} {
		t.Run(string(level), func(t *testing.T) {
			nodes := &localNode{root: t.TempDir(), state: t.TempDir(), level: string(level)}
			store, p := newStore(t, nodes, project.Home{Node: "home", Path: t.TempDir()})
			p.Level = level
			if err := store.projects.Declare(t.Context(), []project.Project{p}); err != nil {
				t.Fatal(err)
			}
			p, _, _ = store.projects.Get(t.Context(), p.ID)
			write(t, p.Home.Path, "original", "pinned node base")
			if _, _, err := store.SnapshotCanonical(t.Context(), p, "", "fixture", "baseline"); err != nil {
				t.Fatal(err)
			}
			var baseline attempt.RecoveryBaseline
			if err := store.ledger.Update(t.Context(), func(tx *ledger.Tx) error { var err error; baseline, err = store.RecoveryBaselineTx(tx, p); return err }); err != nil {
				t.Fatal(err)
			}
			r := attempt.WorkspaceRecovery{ID: "workspace-recovery-" + strings.Repeat("b", 32), Revision: 1, Phase: "recorded", CreatedAt: time.Now().UTC(), RequestedBy: "owner", Project: p.ID, Declaration: project.RecoveryIdentity(p), Target: p.Home, Baseline: baseline, Sources: []attempt.RecoverySource{{Attempt: "source", Task: "1", Revision: 1, At: time.Now().UTC()}}, Head: attempt.RecoveryHead{Artifact: baseline.Artifact, ContentID: baseline.ContentID, Storage: baseline.Storage, Evidence: baseline.Evidence, Version: 1}}
			r.Sources[0].At = r.CreatedAt
			identity := sha256.Sum256([]byte(r.Sources[0].Attempt + "\x00" + r.Target.Node + "\x00" + path.Clean(r.Target.Path)))
			r.ID = "workspace-recovery-" + hex.EncodeToString(identity[:16])
			if level == datalevel.Sealed {
				if _, err := store.PlanRecoveryWorkspace(t.Context(), r, "elsewhere"); err == nil {
					t.Fatal("sealed recovery was placed away from its home")
				}
			} else {
				nodes.level = "public"
				if _, err := store.PlanRecoveryWorkspace(t.Context(), r, "elsewhere"); err == nil {
					t.Fatal("recovery data was placed on a lower-grade node")
				}
				nodes.level = "restricted"
			}
			ws, err := store.PlanRecoveryWorkspace(t.Context(), r, "home")
			if err != nil {
				t.Fatal(err)
			}
			r.Workspace, r.Phase = ws, "materializing"

			original := attempt.Record{Spec: attempt.Spec{ID: r.Sources[0].Attempt, TaskID: r.Sources[0].Task, Project: r.Project, Workspace: project.Workspace{Project: r.Project, Node: r.Target.Node, Path: r.Target.Path, Kind: project.KindCanonical}}, State: attempt.Failed, Revision: 1, Abandoned: &attempt.Abandoned{WorkspaceRecoveryID: r.ID, At: r.CreatedAt, By: r.RequestedBy, ForceStopRevision: r.Sources[0].Revision}}
			if _, err := store.ledger.Begin(t.Context(), original.ID, "attempt", string(original.State), "fixture", original); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ledger.Begin(t.Context(), r.ID, "workspace-recovery", r.Phase, "owner", r); err != nil {
				t.Fatal(err)
			}
			write(t, p.Home.Path, "original", "unsettled current node directory")
			if err := store.PrepareRecoveryWorkspace(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			if read(t, ws.Path, "original") != "pinned node base" {
				t.Fatal("node preparation copied unsettled disk rather than the fixed artifact")
			}
			if read(t, p.Home.Path, "original") != "unsettled current node directory" {
				t.Fatal("node preparation changed the original directory")
			}
			write(t, ws.Path, "original", "working copy data")
			if err := store.PrepareRecoveryWorkspace(t.Context(), r); err == nil {
				t.Fatal("late node preparation reset a working copy")
			}
			if read(t, ws.Path, "original") != "working copy data" {
				t.Fatal("rejected preparation overwrote working node data")
			}
		})
	}
}
