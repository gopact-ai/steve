package delegate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/view"
)

func TestDelegationSettlesReportedUsageOnSuccessAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		for _, reported := range []bool{false, true} {
			w := newWorld(t)
			w.running(t, "codex")
			p := view.Progress{Settings: view.Settings{Model: "model"}, Usage: view.Usage{Reported: reported, ContextTokens: 500}}
			if failed {
				p.Usage.InputTokens, p.Usage.OutputTokens = 100, 20
			}
			w.sessions.run = func(_ context.Context, progress func(view.Progress)) (string, error) {
				progress(p)
				if failed {
					return "partial", errors.New("failed after spending")
				}
				return "done", nil
			}
			out, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "work", Agent: "builder"})
			if (err != nil) != failed {
				t.Fatalf("delegate=%+v err=%v", out, err)
			}
			records, err := w.attempts.Closed(t.Context())
			if err != nil || len(records) != 1 || records[0].Usage == nil || *records[0].Usage != *attemptUsage(p) {
				t.Fatalf("closed=%+v err=%v", records, err)
			}
			if (records[0].State == attempt.Bound) == failed {
				t.Fatalf("state=%s failed=%v", records[0].State, failed)
			}
			_, bound, err := w.service.artifacts.Resolve(t.Context(), "steve/"+out.TaskID+"/result")
			if err != nil || bound == failed {
				t.Fatalf("bound=%v failed=%v err=%v", bound, failed, err)
			}
		}
	}
}

func TestDelegationCompletionFailureDoesNotLandOrDiscard(t *testing.T) {
	w := newWorld(t)
	w.running(t, "codex")
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	db, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER refuse_completion BEFORE UPDATE OF state ON operations WHEN NEW.kind = 'attempt' AND NEW.state = 'bound' BEGIN SELECT RAISE(FAIL, 'completion rejected'); END`); err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	home := t.TempDir()
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: home}}}); err != nil {
		t.Fatal(err)
	}
	art := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	att := attempt.New(book)
	w.service.SetLedger(att, art)
	w.service.workspaces = art
	w.sessions.run = func(_ context.Context, progress func(view.Progress)) (string, error) {
		live, err := att.Live(t.Context())
		if err != nil || len(live) != 1 {
			return "", errors.New("expected one attempt")
		}
		if err := os.WriteFile(filepath.Join(live[0].Workspace.Path, "result.txt"), []byte("result"), 0600); err != nil {
			return "", err
		}
		progress(view.Progress{Usage: view.Usage{Reported: true, InputTokens: 100}})
		return "done", nil
	}
	out, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "work", Agent: "builder"})
	if err == nil || out.State != "failed" {
		t.Fatalf("failed commit reported success: %+v err=%v", out, err)
	}
	records, err := att.ForTask(t.Context(), out.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Failed || records[0].Result == nil || records[0].Usage == nil || records[0].Usage.Input != 100 {
		t.Fatalf("failed completion not closed with candidate/spend: records=%+v err=%v", records, err)
	}
	if live, err := att.Live(t.Context()); err != nil || len(live) != 0 {
		t.Fatalf("failed completion left a live attempt: %+v %v", live, err)
	}
	for _, lease := range records[0].Leases {
		if err := book.Check(t.Context(), lease); !errors.Is(err, ledger.ErrStale) {
			t.Fatalf("failed completion retained lease: %+v %v", lease, err)
		}
	}
	if _, err := os.Stat(filepath.Join(records[0].Workspace.Path, "result.txt")); err != nil {
		t.Fatalf("uncommitted work discarded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("uncommitted work landed: %v", err)
	}
	if _, found, err := art.Resolve(t.Context(), "steve/"+out.TaskID+"/result"); err != nil || found {
		t.Fatalf("failed commit bound name: found=%v err=%v", found, err)
	}
	if pending, err := book.Bindings(t.Context(), "pending-landing"); err != nil || len(pending) != 0 {
		t.Fatalf("failed commit queued landing: %v err=%v", pending, err)
	}
}
