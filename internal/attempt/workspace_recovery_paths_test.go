package attempt

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func recoveryPathEpisode(target, work string) WorkspaceRecovery {
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	p := project.Project{ID: "p", Home: project.Home{Node: "node", Path: target}}
	id := workspaceRecoverySourceID("source", p.Home)
	base := strings.Repeat("a", 40)
	e := contentreplica.GitStorageEvidence{Project: p.ID, Artifact: base, Storage: "standalone", Level: "internal", HomeNode: "node"}
	return WorkspaceRecovery{
		ID: id, Revision: 1, Phase: "ready", CreatedAt: at, RequestedBy: "owner",
		Project: p.ID, Declaration: project.RecoveryIdentity(p), Target: p.Home,
		Baseline: RecoveryBaseline{Name: "project/p/canonical", Version: 1, Artifact: base, Storage: e.Storage, Evidence: e.ID()},
		Head:     RecoveryHead{Artifact: base, Version: 1, Storage: e.Storage, Evidence: e.ID()},
		Sources:  []RecoverySource{{Attempt: "source", Task: "task", Revision: 1, At: at}},
		Workspace: project.Workspace{
			ID: "copy", RecoveryID: id, Project: p.ID, Node: "node", Kind: project.KindWorktree, Path: work, Base: base,
		},
	}
}

func TestRecoveryValidationUsesQualifiedMetadataLocations(t *testing.T) {
	for _, tc := range []struct {
		name, target, work string
		valid              bool
	}{
		{"drive", `C:\root\repo`, `C:\root\container\work`, true},
		{"drive-slashes", `C:/root/repo`, `C:/root/container/work`, true},
		{"unc", `\\server\share\repo`, `\\server\share\container\work`, true},
		{"posix", `/srv/repo`, `/srv/container/work`, true},
		{"posix-literal-backslash", `/srv/a\b`, `/srv/c\d/work`, true},
		{"drive-relative-target", `C:repo`, `C:\root\container\work`, false},
		{"root-relative-target", `\repo`, `C:\root\container\work`, false},
		{"incomplete-unc-target", `\\server`, `C:\root\container\work`, false},
		{"device-target", `\\?\C:\repo`, `C:\root\container\work`, false},
		{"drive-relative-work", `/srv/repo`, `C:work`, false},
		{"root-relative-work", `/srv/repo`, `\root\work`, false},
		{"not-work", `/srv/repo`, `/srv/container/other`, false},
		{"literal-backslash-work-name", `/srv/repo`, `/srv/container\work`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspaceRecovery(recoveryPathEpisode(tc.target, tc.work))
			if (err == nil) != tc.valid {
				t.Fatalf("qualified recovery metadata: valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestRecoveryWriterEqualityUsesQualifiedMetadataPaths(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{`C:\root\work`, `C:/root/temp/../work`, true},
		{`\\server\share\work`, `\\server\share\temp\..\work`, true},
		{`C:\root\work`, `D:\root\work`, false},
		{`\\server\share\work`, `\\server\other\work`, false},
		{`/srv/a\b`, `/srv/a/b`, false},
		{`/srv/a\..\b`, `/srv/b`, false},
		{`C:\root\work`, `C:root\work`, false},
		{"", "", false},
	} {
		if got := samePhysicalPath(tc.a, tc.b); got != tc.same {
			t.Errorf("writer equality(%q, %q)=%v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

func TestRecoveryAdmissionKeepsQualifiedWindowsWriterHeld(t *testing.T) {
	s, _ := newService(t)
	r := Record{
		Spec:  Spec{ID: "writer", Project: "p", Workspace: project.Workspace{ID: "canonical:p", Project: "p", Node: "node", Path: `C:\root\repo`, Kind: project.KindCanonical}},
		State: Running, Unsettled: true,
	}
	if _, err := s.l.Begin(t.Context(), r.ID, kind, string(r.State), "fixture", r); err != nil {
		t.Fatal(err)
	}
	err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
		return CheckWriterTx(tx, "node", `C:/root/temp/../repo`)
	})
	if !errors.Is(err, ErrStopConfirmationRequired) {
		t.Fatalf("qualified alias bypassed real writer guard: %v", err)
	}
}

func storeRecoveryPathEpisode(t *testing.T, s *Service, r WorkspaceRecovery) {
	t.Helper()
	p := project.Project{ID: r.Project, Home: r.Target}
	if err := project.Open(s.l).Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	e := contentreplica.GitStorageEvidence{Project: r.Project, Artifact: r.Baseline.Artifact, Storage: "standalone", Level: "internal", HomeNode: r.Target.Node}
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
		return tx.PutBinding(contentreplica.GitStorageEvidenceKind, e.ID(), e)
	}); err != nil {
		t.Fatal(err)
	}
	original := Record{
		Spec:  Spec{ID: "source", TaskID: "task", Project: r.Project, Workspace: project.Workspace{Project: r.Project, Kind: project.KindCanonical, Node: r.Target.Node, Path: r.Target.Path}},
		State: Failed,
		Abandoned: &Abandoned{
			WorkspaceRecoveryID: r.ID, At: r.CreatedAt, By: r.RequestedBy, ForceStopRevision: 1, ProjectedAt: r.CreatedAt,
		},
	}
	if _, err := s.l.Begin(t.Context(), original.ID, kind, string(original.State), "fixture", original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Begin(t.Context(), r.ID, workspaceRecoveryKind, r.Phase, "fixture", r); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryGuardsProtectQualifiedWindowsContainerUntilRemoved(t *testing.T) {
	for _, location := range []struct{ name, target, work, container, within, sibling string }{
		{"drive", `C:\root\repo`, `C:\root\container\work`, `C:/root/container`, `C:/root/container/marker`, `C:/root/container-other`},
		{"unc", `\\server\share\repo`, `\\server\share\container\work`, `\\server\share\container`, `\\server\share\container\marker`, `\\server\share\container-other`},
	} {
		for _, phase := range []string{"ready", "released-pending", "released-removed"} {
			t.Run(location.name+"/"+phase, func(t *testing.T) {
				s, _ := newService(t)
				r := recoveryPathEpisode(location.target, location.work)
				if phase != "ready" {
					r.Phase, r.FrozenHeadVersion, r.ReleasedAt = "released", 1, r.CreatedAt
					content := RecoveryContent{Storage: r.Baseline.Storage, Evidence: r.Baseline.Evidence}
					r.Residual = &RecoveryResidual{Artifact: r.Baseline.Artifact, RecoveryContent: content, CapturedAt: r.CreatedAt}
					r.Result = &RecoveryResult{Artifact: r.Baseline.Artifact, Version: 1, Landing: "landing", RecoveryContent: content}
				}
				if phase == "released-removed" {
					r.CopyRemovedAt = r.CreatedAt
				}
				storeRecoveryPathEpisode(t, s, r)
				if err := s.l.Read(t.Context(), func(tx *ledger.ReadTx) error {
					// Decode and validate real source/evidence records, not a fake guard Reader.
					if _, err := WorkspaceRecoveryTx(tx, r.ID); err != nil {
						return err
					}
					held := phase != "released-removed"
					for _, directory := range []string{location.container, location.work} {
						err := RecoveryCopyHoldTx(tx, "node", directory)
						if errors.Is(err, ErrWorkspaceRecovery) != held || !held && err != nil {
							t.Fatalf("copy hold %q: %v, held=%v", directory, err, held)
						}
					}
					if err := RecoveryCopyHoldTx(tx, "different-node", location.container); err != nil {
						t.Fatalf("copy guard crossed nodes: %v", err)
					}
					desired := []project.Project{{ID: r.Project, Home: r.Target}, {ID: "other", Home: project.Home{Node: "node", Path: location.within}}}
					err := checkRecoveryDeclarationsTx(tx, desired)
					if errors.Is(err, ErrWorkspaceRecovery) != held || !held && err != nil {
						t.Fatalf("declaration inside container: %v, held=%v", err, held)
					}
					desired[1].Home.Path = location.sibling
					if err := checkRecoveryDeclarationsTx(tx, desired); err != nil {
						t.Fatalf("similar sibling refused: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRecoverySourceFingerprintStaysHistorical(t *testing.T) {
	const historical = "workspace-recovery-300e74179ec7e22646feaaf57cb86aad"
	if got := workspaceRecoverySourceID("source", project.Home{Node: "node", Path: `C:\root\temp\..\repo`}); got != historical {
		t.Fatalf("persisted source hash changed: %s", got)
	}
}
