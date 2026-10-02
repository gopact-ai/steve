package contentreplica_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactpkg "github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func recoveryRetentionFixture(t *testing.T) (retentionOwnerFixture, attempt.WorkspaceRecovery) {
	t.Helper()
	f := newRetentionOwnerFixture(t, "artifact")
	projects := project.Open(f.book)
	p := project.Project{ID: "p", Home: project.Home{Node: "a", Path: "/original/p"}, Repo: project.RepoInPlace, Level: datalevel.Internal, DurablePlaces: []string{"a"}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = projects.Get(t.Context(), p.ID)
	store := artifactpkg.New(t.TempDir(), f.book, projects, nil)
	store.SetReplication(f.client)
	if _, err := store.Bind(t.Context(), artifactpkg.CanonicalRef(p.ID), 0, f.content.Object.Key); err != nil {
		t.Fatal(err)
	}
	var baseline attempt.RecoveryBaseline
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { var err error; baseline, err = store.RecoveryBaselineTx(tx, p); return err }); err != nil {
		t.Fatal(err)
	}
	r := attempt.WorkspaceRecovery{ID: "workspace-recovery-" + strings.Repeat("a", 32), Revision: 1, Phase: "recorded", CreatedAt: time.Now().UTC(), RequestedBy: "owner", Project: p.ID, Declaration: project.RecoveryIdentity(p), Target: p.Home,
		Baseline: baseline,
		Sources:  []attempt.RecoverySource{{Attempt: "source", Task: "1", Revision: 1, At: time.Now().UTC()}},
		Head:     attempt.RecoveryHead{Artifact: f.content.Object.Key, Version: 1, ContentID: f.content.ID, Storage: "replicated", Evidence: baseline.Evidence}}
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

func TestRecoveryRetentionKeepsBaselineIntermediateAndLatestAcceptedContent(t *testing.T) {
	f, r := recoveryRetentionFixture(t)
	r.Phase = "ready"
	r.Workspace = project.Workspace{RecoveryID: r.ID, ID: "copy", Project: r.Project, Node: "a", Path: filepath.Join(t.TempDir(), "work"), Kind: project.KindWorktree, Base: r.Baseline.Artifact}
	parent := r.Baseline.Artifact
	for n, artifact := range []string{strings.Repeat("2", 40), strings.Repeat("3", 40)} {
		data := []byte("accepted output " + artifact)
		m, err := f.client.PrepareBundle(t.Context(), "p", artifact, "", checkpoint.Reference(data), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		var accepted attempt.RecoveryContent
		if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
			if _, err := contentreplica.Record(tx, m); err != nil {
				return err
			}
			owner := artifactpkg.Manifest{ID: artifact, Project: "p", Label: datalevel.Internal, Parent: parent, Content: &m}
			if err := tx.PutBinding("artifact", artifact, owner); err != nil {
				return err
			}
			projects := project.Open(f.book)
			store := artifactpkg.New(t.TempDir(), f.book, projects, nil)
			store.SetReplication(f.client)
			// Pin via a verified named artifact in this fixture transaction, then validate its parent.
			if _, err := tx.CompareAndSetName(artifactpkgCanonical(), int64(n+1), artifact); err != nil {
				return err
			}
			p, err := project.ReadTx(tx, "p")
			if err != nil {
				return err
			}
			if _, err := store.RecoveryBaselineTx(tx, p); err != nil {
				return err
			}
			accepted, err = store.RecoveryOutputTx(tx, "p", parent, artifact)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		mayWrite := true
		r.Head.Sources = append(r.Head.Sources, attempt.RecoveryProducer{NativeMayWrite: &mayWrite, Artifact: artifact, ContentID: m.ID, Storage: "replicated", Evidence: accepted.Evidence, Attempt: "writer-" + artifact[:1], Execution: task.ExecutionToken{TaskID: artifact[:1], Epoch: 1}, Base: parent, HeadVersion: int64(n + 1)})
		r.Head.Artifact, r.Head.ContentID, r.Head.Version, r.Head.Evidence = artifact, m.ID, int64(n+2), accepted.Evidence
		yes := true
		producer := r.Head.Sources[len(r.Head.Sources)-1]
		record := attempt.Record{Spec: attempt.Spec{ID: producer.Attempt, TaskID: producer.Execution.TaskID, Project: r.Project, Execution: &producer.Execution, Workspace: r.Workspace, Base: parent, WorkspaceRecovery: &attempt.RecoveryExecution{ID: r.ID, HeadVersion: producer.HeadVersion}}, State: attempt.Bound, Revision: 1, SessionSettled: &yes, Result: &attempt.Result{Artifact: artifact, RecoveryOutput: &attempt.RecoveryOutput{Name: "steve/" + producer.Execution.TaskID}}}
		if _, err := f.book.Begin(t.Context(), record.ID, "attempt", string(record.State), "fixture", record); err != nil {
			t.Fatal(err)
		}
		if err := f.book.DeleteBinding(t.Context(), "artifact", artifact); err != nil {
			t.Fatal(err)
		}
		parent = artifact
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE operations SET data=?,state=? WHERE id=?`, string(raw), r.Phase, r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{r.Baseline.ContentID, r.Head.Sources[0].ContentID, r.Head.ContentID} {
		if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, id) }); !errors.Is(err, contentreplica.ErrReferenced) {
			t.Fatalf("continuous recovery history was retired: %s %v", id, err)
		}
	}
	for _, store := range []*contentreplica.Store{f.receiver} {
		if result, err := store.GC(t.Context(), f.book); err != nil || result.Blobs != 0 {
			t.Fatalf("recovery history collected: %+v %v", result, err)
		}
	}
	if err := f.receiver.Get(t.Context(), f.content.Object, &bytes.Buffer{}); err != nil {
		t.Fatalf("baseline bytes lost: %v", err)
	}
	for _, producer := range r.Head.Sources {
		m, found, err := contentreplica.Lookup(t.Context(), f.book, producer.ContentID)
		if err != nil || !found {
			t.Fatalf("history manifest lost: %v %v", found, err)
		}
		if err := f.receiver.Get(t.Context(), m.Object, &bytes.Buffer{}); err != nil {
			t.Fatalf("history bytes lost: %v", err)
		}
	}
	r.Head.Sources[0].ContentID = strings.Repeat("f", 64)
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE operations SET data=?,state=? WHERE id=?`, string(raw), r.Phase, r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.receiver.GC(t.Context(), f.book); !errors.Is(err, contentreplica.ErrIntegrity) {
		t.Fatalf("missing historical content did not close collection: %v", err)
	}
}

func TestRecoveryBaselineRejectsMissingCurrentContentManifest(t *testing.T) {
	f := newRetentionOwnerFixture(t, "artifact")
	projects := project.Open(f.book)
	p := project.Project{ID: "p", Level: datalevel.Internal, Repo: project.RepoInPlace, Home: project.Home{Path: t.TempDir()}, DurablePlaces: []string{"a"}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	s := artifactpkg.New(t.TempDir(), f.book, projects, nil)
	s.SetReplication(f.client)
	if _, err := s.Bind(t.Context(), artifactpkg.CanonicalRef(p.ID), 0, f.content.Object.Key); err != nil {
		t.Fatal(err)
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec("DELETE FROM bindings WHERE kind=? AND id=?", contentreplica.ManifestKind, f.content.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Manifest(t.Context(), f.content.Object.Key); !errors.Is(err, contentreplica.ErrIncomplete) {
		t.Fatalf("fixture did not lose real content row: %v", err)
	}
	err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		base, err := s.RecoveryBaselineTx(tx, p)
		if err == nil {
			t.Errorf("accepted a recovery base with a missing current content manifest: %+v", base)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRejectsReplicatedContentDisguisedAsStandalone(t *testing.T) {
	f, r := recoveryRetentionFixture(t)
	r.Baseline.ContentID, r.Head.ContentID = "", ""
	r.Baseline.Storage, r.Head.Storage = "standalone", "standalone"
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec("UPDATE operations SET data=? WHERE id=?", string(raw), r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = f.book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, f.content.ID) })
	if !errors.Is(err, contentreplica.ErrIntegrity) {
		t.Fatalf("storage-mode corruption did not close retirement: %v", err)
	}
	if _, err := f.receiver.GC(t.Context(), f.book); !errors.Is(err, contentreplica.ErrIntegrity) {
		t.Fatalf("storage-mode corruption did not close collection: %v", err)
	}
	if err := f.receiver.Get(t.Context(), f.content.Object, &bytes.Buffer{}); err != nil {
		t.Fatalf("storage-mode corruption removed retained bytes: %v", err)
	}
}

func artifactpkgCanonical() string { return artifactpkg.CanonicalRef("p") }

func TestRecoveryStorageEvidenceIsIndependentAndCannotBeChanged(t *testing.T) {
	for _, which := range []string{"missing", "mode", "project", "artifact", "key", "content"} {
		t.Run(which, func(t *testing.T) {
			f, r := recoveryRetentionFixture(t)
			var proof contentreplica.GitStorageEvidence
			if err := f.book.Read(t.Context(), func(tx *ledger.ReadTx) error {
				var err error
				proof, err = contentreplica.LookupGitStorageEvidence(tx, r.Baseline.Evidence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
				if which == "missing" {
					_, err := tx.Exec(`DELETE FROM bindings WHERE kind=? AND id=?`, contentreplica.GitStorageEvidenceKind, r.Baseline.Evidence)
					return err
				}
				switch which {
				case "mode":
					proof.Storage, proof.ContentID = "standalone", ""
				case "project":
					proof.Project = "foreign"
				case "artifact":
					proof.Artifact = strings.Repeat("9", 40)
				case "content":
					proof.ContentID = strings.Repeat("f", 64)
				case "key":
					_, err := tx.Exec(`UPDATE bindings SET id=? WHERE kind=? AND id=?`, strings.Repeat("f", 64), contentreplica.GitStorageEvidenceKind, r.Baseline.Evidence)
					return err
				}
				raw, err := json.Marshal(proof)
				if err != nil {
					return err
				}
				_, err = tx.Exec(`UPDATE bindings SET data=? WHERE kind=? AND id=?`, string(raw), contentreplica.GitStorageEvidenceKind, r.Baseline.Evidence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { return contentreplica.Retire(tx, f.content.ID) }); !errors.Is(err, contentreplica.ErrIntegrity) {
				t.Fatalf("broken independent evidence allowed retirement: %v", err)
			}
			if _, err := f.receiver.GC(t.Context(), f.book); !errors.Is(err, contentreplica.ErrIntegrity) {
				t.Fatalf("broken independent evidence allowed collection: %v", err)
			}
			if err := f.receiver.Get(t.Context(), f.content.Object, &bytes.Buffer{}); err != nil {
				t.Fatal("broken evidence removed bytes")
			}
		})
	}
}

func TestRecoveryBaselineUsesOnlyCurrentPublishedContentFacts(t *testing.T) {
	for _, which := range []string{"project", "artifact", "unpublished", "released", "receipt", "missing dependency", "cycle", "insufficient copies"} {
		t.Run(which, func(t *testing.T) {
			f := newRetentionOwnerFixture(t, "artifact")
			projects := project.Open(f.book)
			p := project.Project{ID: "p", Home: project.Home{Node: "a", Path: t.TempDir()}, Level: datalevel.Internal, DurablePlaces: []string{"a"}}
			if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
				t.Fatal(err)
			}
			p, _, _ = projects.Get(t.Context(), p.ID)
			store := artifactpkg.New(t.TempDir(), f.book, projects, nil)
			store.SetReplication(f.client)
			if _, err := store.Bind(t.Context(), artifactpkg.CanonicalRef(p.ID), 0, f.content.Object.Key); err != nil {
				t.Fatal(err)
			}
			if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
				m := f.content
				switch which {
				case "project":
					m.Object.Scope.ProjectID = "foreign"
				case "artifact":
					m.Object.Key = strings.Repeat("9", 40)
				case "missing dependency":
					m.Object.Base = strings.Repeat("f", 64)
				case "cycle":
					m.Object.Base = m.ID
				case "insufficient copies":
					m.Receipts = nil
				case "unpublished":
					_, err := tx.Exec(`UPDATE bindings SET data=json_set(data,'$.state','prepared') WHERE kind='content-upload' AND id=?`, m.Receipts[0].UploadID)
					return err
				case "released":
					_, err := tx.Exec(`UPDATE bindings SET data=json_set(data,'$.released',json('true')) WHERE kind='content-receipt' AND id=?`, m.Receipts[0].Key())
					return err
				case "receipt":
					_, err := tx.Exec(`DELETE FROM bindings WHERE kind='content-receipt' AND id=?`, m.Receipts[0].Key())
					return err
				}
				raw, err := json.Marshal(m)
				if err != nil {
					return err
				}
				_, err = tx.Exec(`UPDATE bindings SET data=? WHERE kind=? AND id=?`, string(raw), contentreplica.ManifestKind, f.content.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := store.RecoveryBaselineTx(tx, p); return err }); err == nil {
				t.Fatalf("baseline accepted invalid current owner: %s", which)
			}
		})
	}
}

func TestRecoveryBaselineRejectsARealMissingBundleDependency(t *testing.T) {
	f := newRetentionOwnerFixture(t, "artifact")
	projects := project.Open(f.book)
	p := project.Project{ID: "p", Home: project.Home{Node: "a", Path: t.TempDir()}, Level: datalevel.Internal, DurablePlaces: []string{"a"}}
	if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = projects.Get(t.Context(), p.ID)
	data := []byte("dependent bundle")
	child, err := f.client.PrepareBundle(t.Context(), p.ID, strings.Repeat("2", 40), f.content.ID, checkpoint.Reference(data), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	store := artifactpkg.New(t.TempDir(), f.book, projects, nil)
	store.SetReplication(f.client)
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error {
		if _, err := contentreplica.Record(tx, child); err != nil {
			return err
		}
		return tx.PutBinding("artifact", child.Object.Key, artifactpkg.Manifest{ID: child.Object.Key, Project: p.ID, Label: p.Level, Parent: f.content.Object.Key, Content: &child})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(t.Context(), artifactpkg.CanonicalRef(p.ID), 0, child.Object.Key); err != nil {
		t.Fatal(err)
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := store.RecoveryBaselineTx(tx, p); return err }); err != nil {
		t.Fatalf("valid dependency was not accepted: %v", err)
	}
	if err := f.book.DeleteBinding(t.Context(), contentreplica.ManifestKind, f.content.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := store.RecoveryBaselineTx(tx, p); return err }); !errors.Is(err, contentreplica.ErrIncomplete) {
		t.Fatalf("missing real prerequisite was accepted: %v", err)
	}
}
