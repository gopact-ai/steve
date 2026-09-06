package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestDetachedHeadHistorySurvivesTransfer(t *testing.T) {
	source, home := transferFixture(t)
	run := func(args ...string) string {
		t.Helper()
		out, err := git(t.Context(), home, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "--quiet")
	run("add", ".")
	run("commit", "--quiet", "-m", "base")
	run("checkout", "--quiet", "--detach")
	if err := os.WriteFile(filepath.Join(home, "detached.txt"), []byte("detached commit"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "detached.txt")
	run("commit", "--quiet", "-m", "detached-only")
	want := run("rev-parse", "HEAD")
	bundle := filepath.Join(t.TempDir(), "p.bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	targetHome := filepath.Join(t.TempDir(), "home")
	if _, err := Import(t.Context(), ImportOptions{StateDir: t.TempDir(), HubID: "target", ExpectedSource: "source", Input: bundle, Home: targetHome}); err != nil {
		t.Fatal("detached source cannot import:", err)
	}
	got, err := git(t.Context(), targetHome, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(got)) != want {
		t.Fatalf("HEAD lost: %s %v", got, err)
	}
}

func TestNestedRepositoryHistorySurvivesTransfer(t *testing.T) {
	source, home := transferFixture(t)
	nested := filepath.Join(home, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := git(t.Context(), nested, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "--quiet")
	os.WriteFile(filepath.Join(nested, "work.txt"), []byte("nested"), 0600)
	run("add", ".")
	run("commit", "--quiet", "-m", "nested-history")
	want := run("rev-parse", "HEAD")
	bundle := filepath.Join(t.TempDir(), "p.bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	targetHome := filepath.Join(t.TempDir(), "home")
	if _, err := Import(t.Context(), ImportOptions{StateDir: t.TempDir(), HubID: "target", ExpectedSource: "source", Input: bundle, Home: targetHome}); err != nil {
		t.Fatal(err)
	}
	got, err := git(t.Context(), filepath.Join(targetHome, "nested"), "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(got)) != want {
		t.Fatalf("nested Git history missing: got=%s err=%v", got, err)
	}
}

func TestCompletedSinkReplayAfterTransfer(t *testing.T) {
	source, _ := transferFixture(t)
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	projects.SetHubID("source")
	p, _, err := projects.Get(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	art := artifact.New(filepath.Join(source, "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	ws, err := art.Materialize(t.Context(), project.Request{Project: "p", Isolated: true, Owner: "sink-attempt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "result.txt"), []byte("sink output"), 0600); err != nil {
		t.Fatal(err)
	}
	result, _, err := art.Publish(t.Context(), ws, ws.Base, "sink-attempt", "sink result")
	if err != nil {
		t.Fatal(err)
	}
	id := "plan-sink/1/work/sink-attempt"
	land, err := art.LandOnce(t.Context(), id, p, result.ID, "plan 1")
	if err != nil {
		t.Fatal(err)
	}
	if land.State != artifact.LandCommitted {
		t.Fatalf("land=%+v", land)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	targetHome := filepath.Join(t.TempDir(), "home")
	restored, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: bundle, Home: targetHome})
	if err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects = project.Open(book)
	projects.SetHubID("target")
	art = artifact.New(filepath.Join(target, "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	id = "plan-sink/" + namespace("source", "1") + "/work/sink-attempt"
	if _, err := art.LandOnce(t.Context(), id, restored, result.ID, "plan migrated"); err != nil {
		t.Fatal("committed sink cannot replay after transfer:", err)
	}
}

func TestReleasedProjectCopyProgressAllowsUnchangedSourceStartup(t *testing.T) {
	source, home := transferFixture(t)
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	projects.SetHubID("source")
	copy := project.Copy{Node: "remote", Path: "/copy", Origin: project.OriginCloned, Source: "repository", State: project.CopyProvisioning}
	p := project.Project{ID: "p", Home: project.Home{Path: home}, Copies: map[string]project.Copy{"remote": copy}}
	q := project.Project{ID: "q", Home: project.Home{Path: t.TempDir()}}
	if err := projects.Reconcile(t.Context(), []project.Project{p, q}, "same-config"); err != nil {
		t.Fatal(err)
	}
	copy.State = project.CopyReady
	if err := projects.UpdateDeclaredCopy(t.Context(), "p", copy); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects = project.Open(book)
	projects.SetHubID("source")
	if err := projects.Reconcile(t.Context(), []project.Project{p, q}, "same-config"); err != nil {
		t.Fatal("unchanged source config cannot restart for unrelated q:", err)
	}
}

func TestSourceSymlinkHomePreservesWorkspace(t *testing.T) {
	source, home := transferFixture(t)
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	projects.SetHubID("source")
	p, _, err := projects.Get(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	p.Home.Path = alias
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	targetHome := filepath.Join(t.TempDir(), "home")
	if _, err := Import(t.Context(), ImportOptions{StateDir: t.TempDir(), HubID: "target", ExpectedSource: "source", Input: bundle, Home: targetHome}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(targetHome, "work.txt"))
	if err != nil || string(got) != "current uncommitted content" {
		t.Fatalf("symlink-root project lost workspace: content=%q err=%v", got, err)
	}
}

func TestMemoryAuditSurvivesSecondTransfer(t *testing.T) {
	source, _ := transferFixture(t)
	memoryDir := filepath.Join(source, "memory")
	if err := os.MkdirAll(memoryDir, 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"scope\":{\"kind\":\"project\",\"project\":\"p\"},\"op\":\"remember\",\"id\":\"from-original-hub\"}\n")
	if err := os.WriteFile(filepath.Join(memoryDir, "audit.jsonl"), original, 0600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "first")
	b, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b.Memory["p.audit.jsonl"]) != string(original) {
		t.Fatal("fixture missing first-hop audit")
	}
	target := t.TempDir()
	if _, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: bundle, Home: filepath.Join(t.TempDir(), "home")}); err != nil {
		t.Fatal(err)
	}
	b, err = Export(t.Context(), Options{StateDir: target, HubID: "target", Project: "p", TargetHub: "third", Output: filepath.Join(t.TempDir(), "second"), Evidence: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b.Memory["p.audit.jsonl"]) != string(original) {
		t.Fatalf("second export lost inherited audit: got=%q want=%q", b.Memory["p.audit.jsonl"], original)
	}
}

func TestSnapshotPreservesRawWorktreeBytes(t *testing.T) {
	source, home := transferFixture(t)
	if err := os.WriteFile(filepath.Join(home, ".gitattributes"), []byte("* text eol=lf\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original := []byte("dirty CRLF\r\nsecond line\r\n")
	if err := os.WriteFile(filepath.Join(home, "work.txt"), original, 0600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	targetHome := filepath.Join(t.TempDir(), "home")
	if _, err := Import(t.Context(), ImportOptions{StateDir: t.TempDir(), HubID: "target", ExpectedSource: "source", Input: bundle, Home: targetHome}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(targetHome, "work.txt"))
	if err != nil || string(got) != string(original) {
		t.Fatalf("snapshot normalized original bytes: got=%q want=%q err=%v", got, original, err)
	}
}

func TestDestinationAliasCannotEnterAnotherProject(t *testing.T) {
	source, _ := transferFixture(t)
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	otherHome := t.TempDir()
	book, err := ledger.Open(target, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	projects.SetHubID("target")
	if err := projects.Declare(t.Context(), []project.Project{{ID: "q", Home: project.Home{Path: otherHome}}}); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(otherHome, alias); err != nil {
		t.Fatal(err)
	}
	targetHome := filepath.Join(alias, "migrated-subdir")
	if _, err := Import(t.Context(), ImportOptions{StateDir: target, HubID: "target", ExpectedSource: "source", Input: bundle, Home: targetHome}); err == nil {
		t.Error("import accepted destination nested in another project's home through alias")
	}
	if _, err := os.Stat(filepath.Join(otherHome, "migrated-subdir", "work.txt")); !os.IsNotExist(err) {
		t.Fatalf("foreign project workspace was modified before rejection: %v", err)
	}
}

func TestInterruptedGitRestoreRebuildsIndex(t *testing.T) {
	source, home := transferFixture(t)
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := git(t.Context(), dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	run(home, "init", "--quiet")
	run(home, "add", ".")
	run(home, "commit", "--quiet", "-m", "original")
	bundle := filepath.Join(t.TempDir(), "bundle")
	b, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	stage := t.TempDir()
	if err := restoreSnapshot(t.Context(), b.Workspace, b.Snapshot, dest); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(stage, "history.bundle")
	if err := os.WriteFile(history, b.GitHistory, 0600); err != nil {
		t.Fatal(err)
	}
	// Reach the durable state immediately before restoreGitHistory's reset.
	run(dest, "init", "--quiet")
	run(dest, "fetch", "--quiet", "--update-head-ok", history, "refs/*:refs/*")
	run(dest, "symbolic-ref", "HEAD", b.GitBranch)
	if _, err := os.Stat(filepath.Join(dest, ".git", "index")); !os.IsNotExist(err) {
		t.Fatal("fixture unexpectedly has an index", err)
	}
	if err := restoreGitHistory(t.Context(), b.GitHistory, b.GitCommit, b.GitBranch, dest, stage); err != nil {
		t.Fatal(err)
	}
	got := run(dest, "ls-files")
	if !strings.Contains(got, "work.txt") {
		t.Fatalf("retry accepted matching HEAD with missing Git index: ls-files=%q", got)
	}
}

func TestTaskResultRefsRemapWithoutChangingProse(t *testing.T) {
	source, _ := transferFixture(t)
	book, err := ledger.Open(source, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.SetResult("1", task.Result{Answer: "original task 1 remains prose", Refs: []string{"task 1", "git original-commit"}}); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	b, err := Export(t.Context(), Options{StateDir: source, HubID: "source", Project: "p", TargetHub: "target", Output: bundle, Evidence: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if err := remapBundle(&b, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	r := b.Tasks.Tasks[namespace("source", "1")].Result
	if r.Answer != "original task 1 remains prose" {
		t.Fatal("original prose changed")
	}
	want := "task " + namespace("source", "1")
	if len(r.Refs) == 0 || r.Refs[0] != want {
		t.Fatalf("structured result reference still addresses destination local task 1: got=%q want=%q", r.Refs, want)
	}
}
