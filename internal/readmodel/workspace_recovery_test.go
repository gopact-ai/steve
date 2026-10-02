package readmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestWorkspaceRecoveryRemainsVisibleWithoutALiveExecution(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	base := strings.Repeat("1", 40)
	r := attempt.WorkspaceRecovery{ID: "workspace-recovery-" + strings.Repeat("a", 32), Revision: 1, Phase: "recorded", CreatedAt: time.Now().UTC(), RequestedBy: "owner", Project: "p", Declaration: "fixed", Target: project.Home{Node: "node", Path: "/original"}, Baseline: attempt.RecoveryBaseline{Name: "project/p/canonical", Version: 1, Artifact: base, Storage: "standalone"}, Head: attempt.RecoveryHead{Artifact: base, Version: 1, Storage: "standalone"}, Sources: []attempt.RecoverySource{{Attempt: "old", Task: "1", Revision: 1, At: time.Now().UTC()}}}
	proof := contentreplica.GitStorageEvidence{Project: r.Project, Artifact: base, Storage: "standalone", Level: "internal", HomeNode: r.Target.Node}
	r.Baseline.Evidence, r.Head.Evidence = proof.ID(), proof.ID()
	if err := book.PutBinding(t.Context(), contentreplica.GitStorageEvidenceKind, proof.ID(), proof); err != nil {
		t.Fatal(err)
	}
	r.Sources[0].At = r.CreatedAt
	identity := sha256.Sum256([]byte(r.Sources[0].Attempt + "\x00" + r.Target.Node + "\x00" + path.Clean(r.Target.Path)))
	r.ID = "workspace-recovery-" + hex.EncodeToString(identity[:16])
	original := attempt.Record{Spec: attempt.Spec{ID: r.Sources[0].Attempt, TaskID: r.Sources[0].Task, Project: r.Project, Workspace: project.Workspace{Project: r.Project, Node: r.Target.Node, Path: r.Target.Path, Kind: project.KindCanonical}}, State: attempt.Failed, Revision: 1, Abandoned: &attempt.Abandoned{WorkspaceRecoveryID: r.ID, At: r.CreatedAt, By: r.RequestedBy, ForceStopRevision: r.Sources[0].Revision}}
	if _, err := book.Begin(t.Context(), original.ID, "attempt", string(original.State), "fixture", original); err != nil {
		t.Fatal(err)
	}
	if _, err := book.Begin(t.Context(), r.ID, "workspace-recovery", r.Phase, "owner", r); err != nil {
		t.Fatal(err)
	}
	adapter := Ledger{Book: book}
	got, err := adapter.recoveryWorkspaces(t.Context())
	if err != nil || len(got) != 1 || got[0].ID != r.ID || got[0].Head != base || got[0].Project != "p" {
		t.Fatalf("recovery disappeared with its original attempt: %+v %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "requested_by") || strings.Contains(string(raw), "sources") {
		t.Fatal("workspace summary exposed execution authority")
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec("UPDATE operations SET data='{' WHERE id=?", r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.recoveryWorkspaces(t.Context()); err == nil {
		t.Fatal("unreadable recovery was silently treated as no hold")
	}
}
