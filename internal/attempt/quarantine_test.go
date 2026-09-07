package attempt

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/project"
)

func TestUnconfirmedWriterPreventsDeclarationReassignment(t *testing.T) {
	s, _ := newService(t)
	projects := project.Open(s.l, CheckDeclarationsTx)
	p := project.Project{ID: "p", Home: project.Home{Path: t.TempDir()}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Open(t.Context(), Spec{ID: "unconfirmed", Project: p.ID, Scope: ScopeUnrestricted, Workspace: project.Workspace{ID: "canonical:p", Project: p.ID, Path: p.Home.Path, Kind: project.KindCanonical}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnsettled(t.Context(), r.ID, "test", errors.New("stop not confirmed"), nil); err != nil {
		t.Fatal(err)
	}
	if err := projects.Reconcile(t.Context(), []project.Project{p}, "unchanged"); err != nil {
		t.Fatal(err)
	}
	for _, desired := range [][]project.Project{nil, {{ID: "q", Home: p.Home}}, {{ID: p.ID, Home: project.Home{Path: filepath.Join(t.TempDir(), "moved")}}}, {{ID: "q", Home: project.Home{Path: filepath.Join(p.Home.Path, "nested")}}}} {
		if err := projects.Reconcile(t.Context(), desired, "reassigned"); err == nil {
			t.Fatalf("unconfirmed writer lost physical ownership: %+v", desired)
		}
	}
	if _, err := s.ConfirmStopped(t.Context(), r.ID, "operator", "verified original process exited"); err != nil {
		t.Fatal(err)
	}
	if err := projects.Reconcile(t.Context(), nil, "retired"); err != nil {
		t.Fatal(err)
	}
}
