package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A project directory is inspected the way a person opening it would: a
// repository at the root is one entry; a directory of repositories is one
// entry each, with branch, last commit, dirtiness and whether an
// AGENTS.md is there; a directory that is not there says so.
func TestInspectReposSeesWhatADirectoryHolds(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	mk := func(name string, agents bool, dirty bool) {
		dir := filepath.Join(root, name)
		_ = os.MkdirAll(dir, 0o755)
		run := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		run("init", "-q", "-b", "main")
		_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\n"), 0o644)
		if agents {
			_ = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("read me\n"), 0o644)
		}
		run("add", ".")
		run("commit", "-q", "-m", "first commit of "+name)
		if dirty {
			_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o644)
		}
	}
	mk("alpha", true, false)
	mk("beta", false, true)
	_ = os.MkdirAll(filepath.Join(root, "notes"), 0o755)

	repos := InspectRepos(context.Background(), root)
	if len(repos) != 2 || repos[0].Path != "alpha" || repos[1].Path != "beta" {
		t.Fatalf("repos = %+v", repos)
	}
	if a := repos[0]; a.Branch != "main" || !a.AgentsMD || a.Dirty || a.Subject != "first commit of alpha" || a.Head == "" || a.At.IsZero() {
		t.Fatalf("alpha = %+v", a)
	}
	if b := repos[1]; !b.Dirty || b.AgentsMD {
		t.Fatalf("beta = %+v", b)
	}
	// A directory that is itself a repository is one entry, ".".
	if one := InspectRepos(context.Background(), filepath.Join(root, "alpha")); len(one) != 1 || one[0].Path != "." {
		t.Fatalf("repo root = %+v", one)
	}
	if gone := InspectRepos(context.Background(), filepath.Join(root, "nope")); len(gone) != 1 || !gone[0].Missing {
		t.Fatalf("missing dir = %+v", gone)
	}
}
