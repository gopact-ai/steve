package turn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestTurnCompletionFailureDoesNotPublishItsName(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	p := project.Project{ID: "work", Home: project.Home{Path: t.TempDir()}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	artifacts := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	before, _, err := artifacts.SnapshotCanonical(t.Context(), p, "", "test", "before")
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	r, err := attempts.Open(t.Context(), attempt.Spec{ID: "turn-attempt", TaskID: "task", TurnID: "turn", Project: p.ID, Base: before.ID, Scope: attempt.ScopeUnrestricted, Workspace: project.Workspace{ID: "canonical:work", Kind: project.KindCanonical, Project: p.ID, Path: p.Home.Path}})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []attempt.State{attempt.Prepared, attempt.Running} {
		if _, err := attempts.Advance(t.Context(), r.ID, state, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(p.Home.Path, "result.txt"), []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_turn_bound BEFORE UPDATE ON operations WHEN NEW.state = 'bound' BEGIN SELECT RAISE(ABORT, 'completion rejected'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{attempts: attempts, artifacts: artifacts, projects: projects}
	if err := c.closeAttempt(t.Context(), r.ID, Result{Text: "done"}, nil, &turnSpend{}); err == nil {
		t.Fatal("terminal write failure was reported as success")
	}
	if name, exists, err := artifacts.Resolve(t.Context(), "steve/task/turn/turn"); err != nil || exists {
		t.Fatalf("failed terminal transaction published a name: %+v, %v, %v", name, exists, err)
	}
	live, err := attempts.Live(t.Context())
	if err != nil || len(live) != 0 {
		t.Fatalf("failed completion left a live attempt: %+v, %v", live, err)
	}
	if _, err := attempts.Open(t.Context(), attempt.Spec{ID: "next-turn", Project: p.ID, Scope: attempt.ScopeUnrestricted, Workspace: r.Workspace}); err != nil {
		t.Fatalf("failed completion kept the canonical lease: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Home.Path, "result.txt")); err != nil {
		t.Fatalf("failed completion removed work: %v", err)
	}
}

func TestTurnCompletionRequiresReadableProject(t *testing.T) {
	for _, missing := range []bool{false, true} {
		book, err := ledger.Open(t.TempDir(), ledger.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { book.Close() })
		projects := project.Open(book)
		p := project.Project{ID: "work", Home: project.Home{Path: t.TempDir()}}
		if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
			t.Fatal(err)
		}
		attempts := attempt.New(book)
		r, err := attempts.Open(t.Context(), attempt.Spec{ID: "work", Project: p.ID, Base: "before", Scope: attempt.ScopeUnrestricted, Workspace: p.Canonical()})
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range []attempt.State{attempt.Prepared, attempt.Running} {
			if _, err := attempts.Advance(t.Context(), r.ID, state, "test", nil); err != nil {
				t.Fatal(err)
			}
		}
		if missing {
			err = projects.Retire(t.Context(), p.ID)
		} else {
			err = book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec(`UPDATE bindings SET data = 'not-json' WHERE kind = 'project' AND id = 'work'`)
				return err
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		c := &Coordinator{attempts: attempts, projects: projects, artifacts: artifact.New(t.TempDir(), book, projects, nil)}
		if err := c.closeAttempt(t.Context(), r.ID, Result{Text: "done"}, nil, &turnSpend{}); err == nil || !strings.Contains(err.Error(), "project") {
			t.Fatalf("unread project silently completed: %v", err)
		}
		got, err := attempts.Get(t.Context(), r.ID)
		if err != nil || got.State != attempt.Failed || got.Error == "" {
			t.Fatalf("unread project kept running: %+v, %v", got, err)
		}
		if _, err := attempts.Open(t.Context(), attempt.Spec{ID: "next", Project: p.ID, Scope: attempt.ScopeUnrestricted, Workspace: p.Canonical()}); err != nil {
			t.Fatalf("unread project completion retained canonical lease: %v", err)
		}
	}
}
