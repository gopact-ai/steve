package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func artifactNode(t *testing.T) (*Registry, *Server) {
	t.Helper()
	server := startNode(t, ServerConfig{Name: "n", Token: "tok", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir()})
	registry := NewRegistry("hub", map[string]Config{"n": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	return registry, server
}

func artifactCall(t *testing.T, registry *Registry, name string, req nodewire.ArtifactRequest) nodewire.ArtifactResult {
	t.Helper()
	result, err := registry.Artifact(t.Context(), name, req)
	if err != nil {
		t.Fatalf("%s: %v", req.Op, err)
	}
	return result
}

func artifactWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Only git is on PATH. Any regression to find/du/mkdir/rm/tar/base64 shell
// snippets therefore fails, even on a developer machine with GNU utilities.
func onlyGit(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func TestArtifactOperationsRoundTripWithoutPOSIXTools(t *testing.T) {
	onlyGit(t)
	registry, server := artifactNode(t)
	for _, name := range []string{"", "n"} {
		t.Run("node="+name, func(t *testing.T) {
			bare := filepath.Join(t.TempDir(), "objects ' $x.git")
			call := func(req nodewire.ArtifactRequest) nodewire.ArtifactResult {
				req.Repo = bare
				return artifactCall(t, registry, name, req)
			}
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactInit})
			work := t.TempDir()
			path := "notes ' $x\n[1].md"
			artifactWrite(t, work, path, "before\n")
			artifactWrite(t, work, "vendored/lib.go", "package lib\n")
			if out, err := exec.Command("git", "init", "--quiet", filepath.Join(work, "vendored")).CombinedOutput(); err != nil {
				t.Fatalf("nested init: %s: %v", out, err)
			}
			base := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, WorkTree: work, Message: "message ' $x\nsecond line"})
			if !base.Changed || len(base.Commit) != 40 {
				t.Fatalf("snapshot = %+v", base)
			}
			if _, err := os.Stat(filepath.Join(work, "vendored", ".git")); err != nil {
				t.Fatalf("user repository modified: %v", err)
			}
			if paths := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactChanged, Commit: base.Commit}).Paths; !reflect.DeepEqual(paths, []string{path}) {
				t.Fatalf("snapshot paths = %q", paths)
			}
			if again := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, WorkTree: work, Parent: base.Commit}); again.Changed || again.Commit != base.Commit {
				t.Fatalf("unchanged snapshot = %+v", again)
			}
			checkout := filepath.Join(t.TempDir(), "checkout ' $x")
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactCheckout, Commit: base.Commit, WorkTree: checkout})
			if _, err := registry.Artifact(t.Context(), name, nodewire.ArtifactRequest{Op: nodewire.ArtifactCheckout, Repo: bare, Commit: base.Commit, WorkTree: checkout}); err == nil {
				t.Fatal("checkout overwrote a nonempty directory")
			}
			artifactWrite(t, work, path, "after\n")
			flattened := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, WorkTree: work, Parent: base.Commit, Flatten: true})
			if _, err := os.Lstat(filepath.Join(work, "vendored", ".git")); !os.IsNotExist(err) {
				t.Fatalf("platform nested repository not flattened: %v", err)
			}
			_, err := registry.Artifact(t.Context(), name, nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, Repo: bare, WorkTree: work, Flatten: true, Limits: nodewire.SnapshotLimits{MaxFiles: 1}})
			var limit artifact.TooLarge
			if !errors.As(err, &limit) || limit != (artifact.TooLarge{Which: "files", Have: 2, Limit: 1}) {
				t.Fatalf("limit type lost: %T %v", err, err)
			}
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactApply, From: base.Commit, Commit: flattened.Commit, WorkTree: checkout})
			for entry, want := range map[string]string{path: "after\n", "vendored/lib.go": "package lib\n"} {
				if body, err := os.ReadFile(filepath.Join(checkout, entry)); err != nil || string(body) != want {
					t.Fatalf("applied %q = %q, %v", entry, body, err)
				}
			}
			blob := filepath.Join(server.BlobDir(), "roundtrip.bundle")
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactBundle, Commit: flattened.Commit, Path: blob})
			if name != "" {
				var data bytes.Buffer
				if err := registry.GetBlob(t.Context(), name, "roundtrip.bundle", &data); err != nil {
					t.Fatal(err)
				}
				if err := registry.PutBlob(t.Context(), name, "copy.bundle", bytes.NewReader(data.Bytes()), int64(data.Len())); err != nil {
					t.Fatal(err)
				}
				blob = filepath.Join(server.BlobDir(), "copy.bundle")
			}
			bare = filepath.Join(t.TempDir(), "receiver.git")
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactInit})
			if call(nodewire.ArtifactRequest{Op: nodewire.ArtifactHas, Commit: flattened.Commit}).Has {
				t.Fatal("empty repository has the commit")
			}
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactUnbundle, Path: blob})
			if !call(nodewire.ArtifactRequest{Op: nodewire.ArtifactHas, Commit: flattened.Commit}).Has {
				t.Fatal("bundle lost its tip")
			}
			restored := t.TempDir()
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactCheckout, Commit: flattened.Commit, WorkTree: restored})
			if body, err := os.ReadFile(filepath.Join(restored, "vendored/lib.go")); err != nil || string(body) != "package lib\n" {
				t.Fatalf("bundle round trip: %q, %v", body, err)
			}
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactRemove, Path: blob})
			if _, err := os.Lstat(blob); !os.IsNotExist(err) {
				t.Fatalf("bundle cleanup: %v", err)
			}
			_, err = registry.Artifact(t.Context(), name, nodewire.ArtifactRequest{Op: nodewire.ArtifactCheckout, Repo: bare, Commit: strings.Repeat("f", 40), WorkTree: t.TempDir()})
			var gitErr *artifact.GitError
			if !errors.As(err, &gitErr) || gitErr.Code == 0 || gitErr.Command != "read-tree" {
				t.Fatalf("git status type lost: %T %v", err, err)
			}
		})
	}
}

func TestArtifactMergeConflictTypeAcrossWire(t *testing.T) {
	onlyGit(t)
	registry, _ := artifactNode(t)
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			bare := filepath.Join(t.TempDir(), "objects.git")
			call := func(req nodewire.ArtifactRequest) nodewire.ArtifactResult {
				req.Repo = bare
				return artifactCall(t, registry, "n", req)
			}
			call(nodewire.ArtifactRequest{Op: nodewire.ArtifactInit})
			work := t.TempDir()
			path := "conflict\nwith ' spaces"
			artifactWrite(t, work, path, "base\n")
			base := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, WorkTree: work}).Commit
			artifactWrite(t, work, path, "ours\n")
			ours := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, WorkTree: work, Parent: base}).Commit
			artifactWrite(t, work, path, "theirs\n")
			theirs := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, WorkTree: work, Parent: base}).Commit
			_, err := registry.Artifact(t.Context(), "n", nodewire.ArtifactRequest{Op: nodewire.ArtifactMerge, Repo: bare, Base: base, Ours: ours, Theirs: theirs, LegacyMerge: legacy})
			var conflict artifact.MergeConflict
			if !errors.As(err, &conflict) || !reflect.DeepEqual(conflict.Paths, []string{path}) {
				t.Fatalf("conflict type or paths lost: %T %v", err, err)
			}
			if body, _ := os.ReadFile(filepath.Join(work, path)); string(body) != "theirs\n" {
				t.Fatal("merge changed the source worktree")
			}
		})
	}
}

// Cancellation closes only this operation stream and never falls back to Exec.
func TestArtifactCancellationAndFeatureNegotiation(t *testing.T) {
	client, peer := net.Pipe()
	hub := nodewire.NewMux(client, true)
	node := nodewire.NewMux(peer, false)
	t.Cleanup(func() { hub.Close(); node.Close() })
	c := &conn{name: "n", mux: hub, advert: nodewire.Advert{}}
	r := NewRegistry("hub", map[string]Config{"n": {}})
	r.live["n"] = c
	t.Cleanup(r.Close)
	_, err := r.Artifact(t.Context(), "n", nodewire.ArtifactRequest{Op: nodewire.ArtifactInit})
	var unsupported *nodewire.OperationFailure
	if !errors.As(err, &unsupported) || unsupported.Code != "unsupported" {
		t.Fatalf("legacy node = %v", err)
	}
	c.setAdvert(nodewire.Advert{Features: []string{nodewire.FeatureArtifact}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := r.Artifact(ctx, "n", nodewire.ArtifactRequest{Op: nodewire.ArtifactInit, Repo: "/objects.git"})
		result <- err
	}()
	stream, err := node.Accept(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stream.Request().Kind != nodewire.StreamArtifact {
		t.Fatal("operation used a command stream")
	}
	var req nodewire.ArtifactRequest
	if err := json.NewDecoder(stream).Decode(&req); err != nil {
		t.Fatal(err)
	}
	opctx, stop := operationContext(t.Context(), stream)
	defer stop()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("operation ignored cancellation")
	}
	select {
	case <-opctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("node did not cancel the operation")
	}
}

func TestArtifactRecoveryAndSweepAcrossWire(t *testing.T) {
	onlyGit(t)
	registry, _ := artifactNode(t)
	bare := filepath.Join(t.TempDir(), "objects.git")
	call := func(req nodewire.ArtifactRequest) nodewire.ArtifactResult {
		req.Repo = bare
		return artifactCall(t, registry, "n", req)
	}
	call(nodewire.ArtifactRequest{Op: nodewire.ArtifactInit})
	work := t.TempDir()
	artifactWrite(t, work, "run ' [1]\n", "before")
	artifactWrite(t, work, "deleted", "gone")
	if err := os.Symlink("old-target", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	base := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, WorkTree: work}).Commit
	artifactWrite(t, work, "run ' [1]\n", "after")
	if err := os.Chmod(filepath.Join(work, "run ' [1]\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(work, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new-target", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	artifactWrite(t, work, "added", "new")
	merged := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactSnapshot, WorkTree: work, Parent: base}).Commit
	recover := t.TempDir()
	call(nodewire.ArtifactRequest{Op: nodewire.ArtifactCheckout, WorkTree: recover, Commit: base})
	for _, path := range []string{"run ' [1]\n", "deleted", "link", "added"} {
		req := nodewire.ArtifactRequest{Op: nodewire.ArtifactPathState, WorkTree: recover, From: base, Commit: merged, Path: path}
		if got := call(req).State; got != "old" {
			t.Fatalf("%q before recovery = %s", path, got)
		}
		call(nodewire.ArtifactRequest{Op: nodewire.ArtifactWritePath, WorkTree: recover, Commit: merged, Path: path})
		if got := call(req).State; got != "merged" {
			t.Fatalf("%q after recovery = %s", path, got)
		}
	}
	if info, err := os.Stat(filepath.Join(recover, "run ' [1]\n")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("recovery lost executable mode: %v", err)
	}
	if link, err := os.Readlink(filepath.Join(recover, "link")); err != nil || link != "new-target" {
		t.Fatalf("recovery lost symlink: %q, %v", link, err)
	}
	artifactWrite(t, recover, "added", "user edit")
	if got := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactPathState, WorkTree: recover, From: base, Commit: merged, Path: "added"}).State; got != "other" {
		t.Fatalf("user edit = %s", got)
	}
	_, err := registry.Artifact(t.Context(), "n", nodewire.ArtifactRequest{Op: nodewire.ArtifactWritePath, Repo: bare, WorkTree: recover, Commit: merged, Path: "../outside"})
	var invalid *nodewire.OperationFailure
	if !errors.As(err, &invalid) || invalid.Code != "invalid_request" {
		t.Fatalf("recovery accepted an escaping path: %v", err)
	}
	worktrees := t.TempDir()
	old := filepath.Join(worktrees, "wt-old ' \n")
	recent := filepath.Join(worktrees, "wt-recent")
	for _, dir := range []string{old, recent, filepath.Join(worktrees, "unrelated")} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	before := time.Now().Add(-artifact.SweepAge)
	if err := os.Chtimes(old, before.Add(-time.Minute), before.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, filepath.Join(worktrees, "wt-link")); err != nil {
		t.Fatal(err)
	}
	paths := call(nodewire.ArtifactRequest{Op: nodewire.ArtifactListWorktrees, WorkTree: worktrees, Before: before}).Paths
	if !reflect.DeepEqual(paths, []string{old}) {
		t.Fatalf("sweep candidates = %q", paths)
	}
	call(nodewire.ArtifactRequest{Op: nodewire.ArtifactRemove, Path: old})
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("sweep removed a recent worktree")
	}
}

func TestArtifactOperationsRemainOutsidePeerGrants(t *testing.T) {
	registry, server := artifactNode(t)
	if err := registry.Grant(t.Context(), "n", "one-blob", "allowed.bundle", time.Minute); err != nil {
		t.Fatal(err)
	}
	socket, err := net.DialTimeout("tcp", server.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := socket.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := nodewire.Dial(socket, nodewire.Hello{Hub: "peer:test", Token: "one-blob", Features: nodewire.Features()}); err != nil {
		t.Fatal(err)
	}
	mux := nodewire.NewMux(socket, true)
	defer mux.Close()
	stream, err := mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamArtifact})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = io.ReadAll(stream)
	if err == nil || err.Error() != nodewire.ExitPrefix+"2" {
		t.Fatalf("blob grant must explicitly refuse an artifact stream: %v", err)
	}
	if _, ok := server.grantedName("one-blob"); ok {
		t.Fatal("peer grant was not consumed")
	}
}
