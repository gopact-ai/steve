package contentreplica_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func recoveryRetentionFixture(t *testing.T) (retentionOwnerFixture, attempt.WorkspaceRecovery) {
	t.Helper()
	f := newRetentionOwnerFixture(t, "artifact")
	p := project.Project{ID: "p", Home: project.Home{Path: "/original/p"}, Repo: project.RepoInPlace}
	r := attempt.WorkspaceRecovery{ID: "workspace-recovery-" + strings.Repeat("a", 32), Revision: 1, Phase: "recorded", CreatedAt: time.Now().UTC(), RequestedBy: "owner", Project: p.ID, Declaration: project.RecoveryIdentity(p), Target: p.Home,
		Baseline: attempt.RecoveryBaseline{Name: "project/p/canonical", Version: 1, Artifact: f.content.Object.Key, ContentID: f.content.ID, Storage: "replicated"},
		Sources:  []attempt.RecoverySource{{Attempt: "source", Task: "1", Revision: 1, At: time.Now().UTC()}},
		Head:     attempt.RecoveryHead{Artifact: f.content.Object.Key, Version: 1, ContentID: f.content.ID, Storage: "replicated"}}
	if _, err := f.book.Begin(t.Context(), r.ID, "workspace-recovery", r.Phase, "owner", r); err != nil {
		t.Fatal(err)
	}
	if err := f.book.DeleteBinding(t.Context(), "artifact", f.key); err != nil {
		t.Fatal(err)
	}
	return f, r
}

func TestWorkspaceRecoveryProtectsItsContentWithoutAnArtifactCatalogRow(t *testing.T) {
	f, _ := recoveryRetentionFixture(t)
	err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, f.content.ID) })
	if !errors.Is(err, contentreplica.ErrReferenced) {
		t.Fatalf("workspace recovery content could be retired: %v", err)
	}
	if result, err := f.receiver.GC(t.Context(), f.book); err != nil || result.Blobs != 0 {
		t.Fatalf("recovery content collected: %+v %v", result, err)
	}
	if err := f.receiver.Get(t.Context(), f.content.Object, &bytes.Buffer{}); err != nil {
		t.Fatalf("recovery lost its durable bytes: %v", err)
	}
}

func TestUnreadableOrForeignRecoveryContentClosesCollection(t *testing.T) {
	for _, which := range []string{"unreadable", "project", "artifact", "missing content"} {
		t.Run(which, func(t *testing.T) {
			f, r := recoveryRetentionFixture(t)
			switch which {
			case "project":
				r.Project = "another"
				r.Baseline.Name = "project/another/canonical"
			case "artifact":
				r.Baseline.Artifact = strings.Repeat("9", 40)
			case "missing content":
				r.Baseline.ContentID = strings.Repeat("f", 64)
			}
			raw, _ := json.Marshal(r)
			if which == "unreadable" {
				raw = []byte("{")
			}
			if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec("UPDATE operations SET data=? WHERE id=?", string(raw), r.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, f.content.ID) }); !errors.Is(err, contentreplica.ErrIntegrity) {
				t.Fatalf("invalid recovery did not prevent retirement: %v", err)
			}
			if _, err := f.receiver.GC(t.Context(), f.book); !errors.Is(err, contentreplica.ErrIntegrity) {
				t.Fatalf("invalid recovery did not close GC: %v", err)
			}
			if err := f.receiver.Get(t.Context(), f.content.Object, &bytes.Buffer{}); err != nil {
				t.Fatal("GC deleted recovery bytes before validating its owner")
			}
		})
	}
}
