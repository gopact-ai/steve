package turn

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"modernc.org/sqlite"
)

// These are owner-boundary integration tests with real SQLite and filesystem
// operations. Native identities come from existing test fixtures, not a running
// Hub: they are deliberately not process/transport E2E tests.

type recoveryCombinationNodes struct {
	artifact.Nodes
	applies int
	removes int
}

func (n *recoveryCombinationNodes) Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	if req.Op == ops.Apply {
		n.applies++
	}
	if req.Op == ops.RemoveRecovery {
		n.removes++
	}
	return n.Nodes.Artifact(ctx, node, req)
}

func combinationJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Include every lease column and row. Recovery callers may exclude only their
// exact episode driver: acquire/release changes its holder/epoch/expiry outside
// the business transaction. A02 has no driver and compares all leases.
func combinationLedger(t *testing.T, c *Coordinator, recoveryEpisode ...string) map[string]json.RawMessage {
	t.Helper()
	if len(recoveryEpisode) > 1 {
		t.Fatal("at most one exact recovery driver may be excluded")
	}
	excluded := ""
	if len(recoveryEpisode) == 1 {
		excluded = "workspace-recovery-driver:" + recoveryEpisode[0]
	}
	out := map[string]json.RawMessage{}
	err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error {
		for name, query := range map[string]string{
			"operations": `SELECT json_group_array(json_object('id',id,'kind',kind,'state',state,'revision',revision,'data',json(data),'updated_at',updated_at)) FROM (SELECT * FROM operations ORDER BY id)`,
			"bindings":   `SELECT json_group_array(json_object('kind',kind,'id',id,'data',json(data),'updated_at',updated_at)) FROM (SELECT * FROM bindings ORDER BY kind,id)`,
			"names":      `SELECT json_group_array(json_object('name',name,'version',version,'artifact',artifact)) FROM (SELECT * FROM names ORDER BY name)`,
			"events":     `SELECT json_group_array(json_object('seq',seq,'operation',operation_id,'revision',revision,'from',from_state,'to',to_state,'actor',actor)) FROM (SELECT * FROM events ORDER BY seq)`,
		} {
			var raw string
			if err := tx.QueryRow(query).Scan(&raw); err != nil {
				return err
			}
			out[name] = json.RawMessage(raw)
		}
		var raw string
		if err := tx.QueryRow(`SELECT json_group_array(json_object('key',resource_key,'incarnation',incarnation,'epoch',epoch,'holder',holder,'expires_at',expires_at)) FROM (SELECT * FROM leases WHERE (?='' OR resource_key<>?) ORDER BY resource_key)`, excluded, excluded).Scan(&raw); err != nil {
			return err
		}
		out["leases"] = json.RawMessage(raw)
		out["excluded_driver_lease"], _ = json.Marshal(excluded)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func combinationFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(full string, entry fs.DirEntry, cause error) error {
		if cause != nil {
			return cause
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			out[rel] = "symlink:" + target
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected physical entity %s", rel)
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func combinationEvidence(t *testing.T, label string, value any) {
	t.Helper()
	root := os.Getenv("STEVE_FAULT_EVIDENCE_ROOT")
	if root == "" {
		return
	}
	if !filepath.IsAbs(root) {
		t.Fatal("fault evidence root must be absolute")
	}
	dir := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "--"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, label+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// Backup only this test's ledger through a separate mode=ro connection. The
// destination contains the committed WAL boundary, never a copied live db file.
func combinationCheckpoint(t *testing.T, c *Coordinator, label string, value any) {
	t.Helper()
	combinationEvidence(t, label, value)
	root := os.Getenv("STEVE_FAULT_EVIDENCE_ROOT")
	if root == "" {
		return
	}
	var source string
	if err := ledgerOf(t, c).DB().QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&source); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", "file:"+filepath.ToSlash(source)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	conn, err := reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	var incarnation int64
	if err := conn.QueryRowContext(t.Context(), "SELECT value FROM meta WHERE key='incarnation'").Scan(&incarnation); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "--"), label+"-ledger-readonly.db")
	err = conn.Raw(func(raw any) error {
		provider, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("test SQLite online backup unavailable")
		}
		backup, err := provider.NewBackup(dest)
		if err != nil {
			return err
		}
		_, stepErr := backup.Step(-1)
		return errors.Join(stepErr, backup.Finish())
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dest, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryFaultCombinationSameSHARevokedNonLastSource(t *testing.T) {
	for _, revoked := range []int{-1, 0, 1} {
		name := "authorized-control"
		if revoked >= 0 {
			name = fmt.Sprintf("revoke-source-%d-of-3", revoked)
		}
		t.Run(name, func(t *testing.T) {
			c, p, source, ws := sharedCopy(t)
			var accepted string
			for index := range 3 {
				id := fmt.Sprintf("same-sha-combination-%d", index)
				spec := recoveryCopySpec(t, c, p, ws, id)
				r, err := c.attempts.Open(t.Context(), spec)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.tasks.BindAttempt(*r.Execution, r.ID, r.TurnID); err != nil {
					t.Fatal(err)
				}
				if _, err := c.attempts.Advance(t.Context(), r.ID, attempt.Prepared, "fixture", nil); err != nil {
					t.Fatal(err)
				}
				if _, err := c.attempts.RecordSession(t.Context(), r.ID, "fixture", "ns_"+id); err != nil {
					t.Fatal(err)
				}
				if _, err := c.attempts.Advance(t.Context(), r.ID, attempt.Running, "fixture", func(r *attempt.Record) { r.NativeContext = "native-" + id }); err != nil {
					t.Fatal(err)
				}
				if index == 0 {
					if err := os.WriteFile(filepath.Join(ws.Path, "same-content"), []byte("one accepted SHA, three producers\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
					t.Fatal(err)
				}
				r, _ = c.attempts.Get(t.Context(), r.ID)
				completion, _, err := c.completion(t.Context(), r, Result{Text: "done"}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if index == 0 {
					accepted = completion.Result.Artifact
				} else {
					// Reuse accepted content through the completion owner, while
					// retaining this producer's exact token and result-name decision.
					name := "steve/" + r.TaskID + "/turn/" + r.TurnID
					completion.Result.Artifact = accepted
					completion.Result.RecoveryOutput = &attempt.RecoveryOutput{Name: name}
					completion.Binding = &attempt.NameBinding{Name: name}
				}
				if _, err := c.attempts.FinishCompletion(t.Context(), r.ID, "fixture", completion); err != nil {
					t.Fatal(err)
				}
			}
			episode := captureBoundCopy(t, c, source)
			if len(episode.Head.Sources) != 3 || episode.Head.Version != 4 {
				t.Fatal("fixture lacks the complete three-producer chain")
			}
			for i, producer := range episode.Head.Sources {
				if producer.Artifact != accepted || producer.Evidence != episode.Head.Sources[0].Evidence || producer.HeadVersion != int64(i)+1 {
					t.Fatal("same-SHA fixture lost content or producing identity")
				}
				if i > 0 && (producer.Attempt == episode.Head.Sources[i-1].Attempt || producer.Execution == episode.Head.Sources[i-1].Execution) {
					t.Fatal("same content deduplicated producing authority")
				}
			}
			if revoked >= 0 {
				if _, err := c.tasks.SetAside(episode.Head.Sources[revoked].Execution.TaskID, task.StateCancelled); err != nil {
					t.Fatal(err)
				}
			}
			if err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error {
				return task.CheckExecutionTx(tx, &episode.Head.Sources[2].Execution)
			}); err != nil {
				t.Fatalf("last source must stay authorized: %v", err)
			}
			nodes := &recoveryCombinationNodes{Nodes: artifact.LocalNodes{Dir: t.TempDir()}}
			c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, nodes)
			c.artifacts.SetExecution(c.executions)
			before := combinationFiles(t, p.Home.Path)
			head, found, err := c.artifacts.Resolve(t.Context(), artifact.CanonicalRef(p.ID))
			if err != nil || !found {
				t.Fatal(err)
			}
			var land artifact.Landing
			err = NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
				var err error
				land, err = c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver)
				return err
			})
			if revoked >= 0 {
				if !errors.Is(err, task.ErrExecutionStopped) || nodes.applies != 0 {
					t.Fatalf("revoked non-last same-SHA source reached applying: land=%+v applies=%d err=%v", land, nodes.applies, err)
				}
				if combinationJSON(t, combinationFiles(t, p.Home.Path)) != combinationJSON(t, before) {
					t.Fatal("revoked same-SHA source wrote original bytes")
				}
				after, _, err := c.artifacts.Resolve(t.Context(), head.Name)
				if err != nil || after != head {
					t.Fatalf("refused admission moved canonical: %+v %v", after, err)
				}
				lands, err := c.artifacts.Landings(t.Context(), p.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, candidate := range lands {
					if candidate.State == artifact.LandApplying || candidate.State == artifact.LandRecoveryPending || candidate.State == artifact.LandCommitted {
						t.Fatal("revoked chain created an admitted WAL or winner")
					}
				}
			} else {
				if err != nil || land.State != artifact.LandCommitted || nodes.applies != 1 {
					t.Fatalf("authorized same-SHA control failed: %+v applies=%d err=%v", land, nodes.applies, err)
				}
				if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "same-content")); err != nil || string(raw) != "one accepted SHA, three producers\n" {
					t.Fatalf("authorized control did not really land: %q %v", raw, err)
				}
			}
			assertRecoveryRegistryIdle(t, c)
			combinationCheckpoint(t, c, "proof", map[string]any{"layer": "Go real-ledger/real-FS integration, not E2E", "sources": episode.Head.Sources, "revoked_index": revoked, "apply_calls": nodes.applies, "before_files": before, "after_files": combinationFiles(t, p.Home.Path), "ledger_read": combinationLedger(t, c, episode.ID)})
		})
	}
}

func TestRecoveryFaultCombinationFinalPendingResultHoldRollback(t *testing.T) {
	for _, refused := range []string{"pending-delete", "released-after-delete", "event-after-released"} {
		t.Run(refused, func(t *testing.T) {
			c, p, episode, stuck := recoveryConflictFixture(t)
			land, err := c.artifacts.ResolveByHand(t.Context(), p, stuck, []artifact.Edit{{Path: "original", Text: "combined atomic release\n"}}, "console")
			if err != nil || land.State != artifact.LandCommitted || land.Committed == nil {
				t.Fatalf("fixture has no actual committed resolution: %+v %v", land, err)
			}
			for name, wanted := range map[string]string{"original": "combined atomic release\n", "inputs/ordinary": "keep ordinary input\n"} {
				if raw, err := os.ReadFile(filepath.Join(p.Home.Path, name)); err != nil || string(raw) != wanted {
					t.Fatalf("committed resolution did not preserve exact %s bytes: %q %v", name, raw, err)
				}
			}
			current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
			if err != nil || current.Phase != "landing" || current.Result != nil {
				t.Fatalf("fixture released before fault boundary: %+v %v", current, err)
			}
			pending, err := ledgerOf(t, c).Bindings(t.Context(), "pending-landing")
			if err != nil || len(pending) != 1 || len(pending[p.ID+"/"+episode.Head.Artifact]) == 0 {
				t.Fatal("fixture must retain the exact root Pending until final release")
			}
			statement := `CREATE TRIGGER refuse_final_release BEFORE DELETE ON bindings WHEN OLD.kind='pending-landing' BEGIN SELECT RAISE(ABORT,'final Pending delete refused'); END`
			reason := "final Pending delete refused"
			if refused == "released-after-delete" {
				statement = `CREATE TRIGGER refuse_final_release BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND NEW.state='released' BEGIN SELECT CASE WHEN EXISTS(SELECT 1 FROM bindings WHERE kind='pending-landing') THEN RAISE(ABORT,'wrong fault boundary: Pending still present') ELSE RAISE(ABORT,'final released refused after Pending deletion') END; END`
				reason = "final released refused after Pending deletion"
			}
			if refused == "event-after-released" {
				statement = `CREATE TRIGGER refuse_final_release BEFORE INSERT ON events WHEN NEW.operation_id='` + episode.ID + `' AND NEW.to_state='released' BEGIN SELECT CASE WHEN EXISTS(SELECT 1 FROM bindings WHERE kind='pending-landing') OR NOT EXISTS(SELECT 1 FROM operations WHERE id=NEW.operation_id AND state='released' AND json_extract(data,'$.result.artifact') IS NOT NULL) THEN RAISE(ABORT,'wrong fault boundary: release not written') ELSE RAISE(ABORT,'final event refused after Result and Pending writes') END; END`
				reason = "final event refused after Result and Pending writes"
			}
			forceStopTrigger(t, c, statement)
			before := combinationLedger(t, c, episode.ID)
			physical := combinationFiles(t, p.Home.Path)
			err = NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
				returned, cause := c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
				if cause == nil || !strings.Contains(cause.Error(), reason) || returned.Phase != "landing" || returned.Result != nil || !returned.ReleasedAt.IsZero() {
					t.Fatalf("final fault was hidden or returned speculative result: %+v %v", returned, cause)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			after := combinationLedger(t, c, episode.ID)
			if combinationJSON(t, before) != combinationJSON(t, after) {
				t.Fatal("refused final transaction partially changed Pending/result/episode/events")
			}
			if err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error { return attempt.RecoveryHoldTx(tx, p.Home.Node, p.Home.Path) }); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
				t.Fatalf("refused final Result lost original hold: %v", err)
			}
			reopened, err := attempt.New(ledgerOf(t, c)).WorkspaceRecovery(t.Context(), episode.ID)
			if err != nil || reopened.Result != nil || reopened.Phase != "landing" || !reopened.ReleasedAt.IsZero() {
				t.Fatalf("cold reader saw speculative release: %+v %v", reopened, err)
			}
			if combinationJSON(t, combinationFiles(t, p.Home.Path)) != combinationJSON(t, physical) {
				t.Fatal("metadata release retried physical landing")
			}
			combinationCheckpoint(t, c, "refusal", map[string]any{"fault_boundary": reason, "before_read": before, "after_read": after, "hold": "retained", "result": nil, "physical": physical})
			forceStopTrigger(t, c, "DROP TRIGGER refuse_final_release")
			err = NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
				released, err := c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
				if err != nil || released.Phase != "released" || released.Result == nil || released.Result.Artifact != land.Committed.Artifact || released.Result.Version != land.Committed.Version || released.Result.Landing != land.ID {
					t.Fatalf("exact final release retry failed: %+v %v", released, err)
				}
				beforeReplay := combinationLedger(t, c, episode.ID)
				again, err := c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
				if err != nil || combinationJSON(t, again) != combinationJSON(t, released) || combinationJSON(t, combinationLedger(t, c, episode.ID)) != combinationJSON(t, beforeReplay) {
					t.Fatal("released retry replaced result or duplicated events")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			pending, err = ledgerOf(t, c).Bindings(t.Context(), "pending-landing")
			if err != nil || len(pending) != 0 {
				t.Fatal("successful final release did not consume its Pending")
			}
			if err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error { return attempt.RecoveryHoldTx(tx, p.Home.Node, p.Home.Path) }); err != nil {
				t.Fatalf("committed release kept original hold: %v", err)
			}
			if combinationJSON(t, combinationFiles(t, p.Home.Path)) != combinationJSON(t, physical) {
				t.Fatal("retry changed already committed canonical bytes")
			}
			combinationCheckpoint(t, c, "retry", map[string]any{"ledger_read": combinationLedger(t, c, episode.ID), "hold": "released", "pending": "consumed", "canonical": land.Committed, "physical": physical})
		})
	}
}

func TestRecoveryFaultCombinationAbandonDescendantTreeRollback(t *testing.T) {
	for _, refused := range []string{"abandon-record", "descendant-record", "accounting-cutoff", "last-control-record"} {
		for _, restart := range []bool{false, true} {
			storeName := "hot-original"
			if restart {
				storeName = "cold-restart"
			}
			t.Run(refused+"/"+storeName, func(t *testing.T) {
				c, tasks, original, _ := forceStopControlFixture(t)
				if err := tasks.BindAttempt(*original.Execution, original.ID, original.TurnID); err != nil {
					t.Fatal(err)
				}
				ids := []string{original.TaskID}
				records := []attempt.Record{}
				for index, parentIndex := range []int{0, 0, 1, 3, -1} {
					parent := ""
					if parentIndex >= 0 {
						parent = ids[parentIndex]
					}
					child, err := tasks.Create(task.Task{Parent: parent, Channel: "console:tree", Transport: "console", Member: "worker", ProjectID: "p"})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tasks.Begin(child.ID, "worker", "node", ""); err != nil {
						t.Fatal(err)
					}
					token, err := tasks.ExecutionToken(child.ID)
					if err != nil {
						t.Fatal(err)
					}
					spec := original.Spec
					spec.ID, spec.TaskID, spec.TurnID = fmt.Sprintf("tree-execution-%d", index), child.ID, fmt.Sprintf("tree-input-%d", index)
					spec.Execution = &token
					spec.Workspace.ID, spec.Workspace.Path = spec.ID, t.TempDir()
					r, err := c.attempts.Open(t.Context(), spec)
					if err != nil {
						t.Fatal(err)
					}
					if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
						t.Fatal(err)
					}
					for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
						r, err = c.attempts.Advance(t.Context(), r.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_" + r.ID })
						if err != nil {
							t.Fatal(err)
						}
					}
					ids = append(ids, child.ID)
					records = append(records, r)
				}
				// Pause revokes the actual tree without cancelling its records.
				// RequestForceStop accepts this already-revoked native execution.
				// AbandonExecution must therefore perform real paused->cancelled
				// descendant mutations, not rely on a prior ForceStop cancellation.
				if _, err := tasks.SetAside(original.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
				if err := c.attempts.MarkUnsettled(t.Context(), original.ID, "fixture", errors.New("original unreachable"), &attempt.Usage{Input: 11, Output: 13, Reported: true}); err != nil {
					t.Fatal(err)
				}
				if _, err := c.attempts.RequestForceStop(t.Context(), original.ID, "owner"); err != nil {
					t.Fatal(err)
				}
				original, err := c.attempts.RecordForceStopResult(t.Context(), original.ID, 1, true, "stop_unproven")
				if err != nil {
					t.Fatal(err)
				}
				beforeTasks := combinationJSON(t, tasks.List(""))
				beforeEpochs := map[string]uint64{}
				for _, id := range ids[:5] {
					tracked, _ := tasks.Get(id)
					beforeEpochs[id] = tracked.ExecutionEpoch
					if tracked.State != task.StatePaused {
						t.Fatalf("AB tree is not really paused: %s %s", id, tracked.State)
					}
				}
				unrelated, _ := tasks.Get(ids[5])
				statement := `CREATE TRIGGER refuse_tree_abandon BEFORE UPDATE ON operations WHEN NEW.id='force-original' AND json_extract(NEW.data,'$.abandoned') IS NOT NULL BEGIN SELECT RAISE(ABORT,'tree AB refused'); END`
				reason := "tree AB refused"
				switch refused {
				case "descendant-record":
					statement = `CREATE TRIGGER refuse_tree_abandon BEFORE UPDATE ON bindings WHEN NEW.kind='task' AND NEW.id='` + ids[4] + `' AND json_extract(NEW.data,'$.state')='cancelled' BEGIN SELECT RAISE(ABORT,'descendant cancellation refused'); END`
					reason = "descendant cancellation refused"
				case "accounting-cutoff":
					statement = `CREATE TRIGGER refuse_tree_abandon BEFORE UPDATE ON bindings WHEN NEW.kind='task-attempt' AND json_extract(NEW.data,'$.accounting_frozen_at') IS NOT NULL BEGIN SELECT RAISE(ABORT,'tree cutoff refused'); END`
					reason = "tree cutoff refused"
				case "last-control-record":
					treeIDs := "'" + strings.Join(ids[:5], "','") + "'"
					statement = `CREATE TRIGGER refuse_tree_abandon BEFORE UPDATE ON bindings WHEN NEW.kind='task-store' BEGIN SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM operations WHERE id='force-original' AND json_extract(data,'$.abandoned') IS NOT NULL) OR EXISTS(SELECT 1 FROM bindings WHERE kind='task' AND id IN (` + treeIDs + `) AND json_extract(data,'$.state')!='cancelled') OR NOT EXISTS(SELECT 1 FROM bindings WHERE kind='task-attempt' AND json_extract(data,'$.task_id')='` + original.TaskID + `' AND json_extract(data,'$.accounting_frozen_at') IS NOT NULL) THEN RAISE(ABORT,'wrong tree final boundary') ELSE RAISE(ABORT,'tree final control refused') END; END`
					reason = "tree final control refused"
				}
				forceStopTrigger(t, c, statement)
				beforeLedger := combinationLedger(t, c)
				returned, cause := NewAbandonControl(c).AbandonAttempt(t.Context(), original.ID, "owner", 1)
				if cause == nil || !strings.Contains(cause.Error(), reason) || returned.Abandoned != nil {
					t.Fatalf("tree AB refusal was hidden: %+v %v", returned, cause)
				}
				afterLedger := combinationLedger(t, c)
				if combinationJSON(t, beforeLedger) != combinationJSON(t, afterLedger) || combinationJSON(t, tasks.List("")) != beforeTasks {
					t.Fatal("AB refusal partially changed tree epochs/cutoff/cache/events")
				}
				cold, err := task.OpenLedger(ledgerOf(t, c))
				if err != nil || combinationJSON(t, cold.List("")) != beforeTasks {
					t.Fatalf("cold task store disagrees after AB refusal: %v", err)
				}
				loaded, err := c.attempts.Get(t.Context(), original.ID)
				if err != nil || combinationJSON(t, loaded) != combinationJSON(t, original) {
					t.Fatalf("AB refusal changed original native execution: %v", err)
				}
				combinationCheckpoint(t, c, "refusal", map[string]any{"fault": refused, "tree": ids[:5], "unrelated": ids[5], "before_read": beforeLedger, "after_read": afterLedger, "cache_and_cold_store_equal": true})
				forceStopTrigger(t, c, "DROP TRIGGER refuse_tree_abandon")
				if restart {
					c.tasks = cold
				} else if c.tasks != tasks {
					t.Fatal("hot retry lost the original failed store")
				}
				retried := c.tasks
				abandoned, err := NewAbandonControl(c).AbandonAttempt(t.Context(), original.ID, "owner", 1)
				if err != nil || abandoned.Abandoned == nil || !abandoned.Unsettled {
					t.Fatalf("valid tree retry failed or invented native exit: %+v %v", abandoned, err)
				}
				for _, id := range ids[:5] {
					after, _ := retried.Get(id)
					if after.State != task.StateCancelled {
						t.Fatalf("retry left descendant %s %s", id, after.State)
					}
					if after.ExecutionEpoch != beforeEpochs[id]+1 {
						t.Fatal("tree retry did not revoke exactly once")
					}
					if id != original.TaskID && (!after.Attempts[0].AccountingFrozenAt.IsZero() || !after.Attempts[0].Open()) {
						t.Fatal("AB froze or closed an unselected descendant execution")
					}
				}
				durable, err := task.OpenLedger(ledgerOf(t, c))
				if err != nil || combinationJSON(t, durable.List("")) != combinationJSON(t, retried.List("")) {
					t.Fatalf("retry cache and durable store disagree: %v", err)
				}
				afterUnrelated, _ := retried.Get(ids[5])
				if combinationJSON(t, unrelated) != combinationJSON(t, afterUnrelated) {
					t.Fatal("AB changed an unrelated task")
				}
				for _, r := range records {
					after, err := c.attempts.Get(t.Context(), r.ID)
					if err != nil || combinationJSON(t, r) != combinationJSON(t, after) {
						t.Fatal("AB fabricated descendant native close/abandon facts")
					}
				}
				if combinationJSON(t, combinationLedger(t, c)["leases"]) != combinationJSON(t, beforeLedger["leases"]) {
					t.Fatal("AB retry changed any lease row, column or expiry")
				}
				root, _ := retried.Get(original.TaskID)
				row := root.Attempts[0]
				if row.AccountingFrozenAt != abandoned.Abandoned.At || row.EndedAt != abandoned.Abandoned.At || row.Tokens.Input != 11 || row.Tokens.Output != 13 {
					t.Fatalf("AB cutoff did not share committed original timestamp: %+v", row)
				}
				beforeRepeat := combinationLedger(t, c)
				again, err := NewAbandonControl(c).AbandonAttempt(t.Context(), original.ID, "owner", 1)
				if err != nil || combinationJSON(t, again.Abandoned) != combinationJSON(t, abandoned.Abandoned) || combinationJSON(t, beforeRepeat) != combinationJSON(t, combinationLedger(t, c)) {
					t.Fatal("duplicate AB re-revoked tree or cutoff")
				}
				for index, r := range records[:4] {
					if err := retried.SettleAttempt(t.Context(), r.TaskID, r.ID, r.TurnID, time.Now().UTC(), task.OutcomeCancelled, task.RecoveryUsage{Tokens: task.Tokens{Input: int64(index + 1), Output: int64(2 * (index + 1))}, Reported: true}); err != nil {
						t.Fatal(err)
					}
				}
				accounted, _ := retried.Get(original.TaskID)
				if accounted.Budget.Tokens.Input != 21 || accounted.Budget.Tokens.Output != 33 || combinationJSON(t, accounted.Attempts[0]) != combinationJSON(t, row) {
					t.Fatalf("late descendant accounting changed the frozen row or lost valid usage: %+v", accounted)
				}
				if combinationJSON(t, combinationLedger(t, c)["leases"]) != combinationJSON(t, beforeLedger["leases"]) {
					t.Fatal("late descendant accounting changed any lease row")
				}
				durable, err = task.OpenLedger(ledgerOf(t, c))
				if err != nil || combinationJSON(t, durable.List("")) != combinationJSON(t, retried.List("")) {
					t.Fatalf("late usage cache and durable store disagree: %v", err)
				}
				combinationCheckpoint(t, c, "retry", map[string]any{"ledger_read": combinationLedger(t, c), "retry_store": storeName, "abandoned": abandoned.Abandoned, "root_cutoff": row, "descendant_native_obligations_retained": true, "outside_tree_unchanged": true})
			})
		}
	}
}

func TestRecoveryFaultCombinationDeleteAckRefusalBasenameReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pinned-directory replacement oracle requires Unix unlink semantics")
	}
	for _, marker := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement-marker-%t", marker), func(t *testing.T) {
			c, p, ws, episode := releasedRecoveryCopy(t)
			container := filepath.Dir(ws.Path)
			// Pin the unlinked inode: immediate allocation must not accidentally
			// reuse its inode and weaken the replacement identity oracle.
			oldHandle, err := os.Open(container)
			if err != nil {
				t.Fatal(err)
			}
			defer oldHandle.Close()
			root := filepath.Dir(filepath.Dir(container))
			local := artifact.LocalNodes{Dir: filepath.Dir(filepath.Dir(root))}
			nodes := &recoveryCombinationNodes{Nodes: local}
			c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, nodes)
			physical := combinationFiles(t, p.Home.Path)
			other := project.Project{ID: "other", Home: project.Home{Node: ws.Node, Path: container}}
			forceStopTrigger(t, c, `CREATE TRIGGER refuse_delete_ack BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND json_extract(NEW.data,'$.copy_removed_at') IS NOT NULL AND json_extract(NEW.data,'$.copy_removed_at')!='0001-01-01T00:00:00Z' BEGIN SELECT RAISE(ABORT,'physical deletion ack refused'); END`)
			err = NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
				returned, cause := c.artifacts.CleanupRecoveryCopy(ctx, episode.ID, driver)
				if cause == nil || !strings.Contains(cause.Error(), "physical deletion ack refused") || !returned.CopyRemovedAt.IsZero() {
					t.Fatalf("ack-only refusal was not reached: %+v %v", returned, cause)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(container); !os.IsNotExist(err) || nodes.removes != 1 {
				t.Fatalf("physical deletion not real/exact: removes=%d err=%v", nodes.removes, err)
			}
			intent, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
			if err != nil || intent.CopyIdentity == "" || intent.CopyRootIdentity == "" || !intent.CopyRemovedAt.IsZero() || intent.Phase != "released" {
				t.Fatalf("delete/ack gap lost its intent and claim: %+v %v", intent, err)
			}
			if err := c.projects.Declare(t.Context(), []project.Project{p, other}); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
				t.Fatalf("delete success guessed acknowledgement: %v", err)
			}
			combinationCheckpoint(t, c, "delete-ack-refused", map[string]any{"recovery": intent, "container_absent": true, "remove_calls": nodes.removes, "ledger_read": combinationLedger(t, c, episode.ID)})
			// Even copying the exact old marker may not confer inode authority.
			if err := os.MkdirAll(ws.Path, 0700); err != nil {
				t.Fatal(err)
			}
			oldInfo, err := oldHandle.Stat()
			if err != nil {
				t.Fatal(err)
			}
			newInfo, err := os.Stat(container)
			if err != nil || os.SameFile(oldInfo, newInfo) {
				t.Fatalf("fixture failed to produce a distinct replacement inode: %v", err)
			}
			if marker {
				if err := os.WriteFile(filepath.Join(container, ".steve-workspace"), []byte(episode.ID+"\n"+episode.Baseline.Artifact+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(ws.Path, "replacement.bin"), []byte("new entity at old basename\x00\xff\n"), 0600); err != nil {
				t.Fatal(err)
			}
			replacement := combinationFiles(t, container)
			forceStopTrigger(t, c, "DROP TRIGGER refuse_delete_ack")
			beforeRetry := combinationLedger(t, c, episode.ID)
			err = NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
				_, err := c.artifacts.CleanupRecoveryCopy(ctx, episode.ID, driver)
				if !errors.Is(err, gitrepo.ErrPreparedWorkspaceChanged) {
					t.Fatalf("replacement directory did not invalidate old intent: %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if nodes.removes != 1 || combinationJSON(t, combinationFiles(t, container)) != combinationJSON(t, replacement) || combinationJSON(t, combinationLedger(t, c, episode.ID)) != combinationJSON(t, beforeRetry) {
				t.Fatal("ack retry erased replacement or withdrew the old claim")
			}
			if err := c.projects.Declare(t.Context(), []project.Project{p, other}); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
				t.Fatalf("same-basename replacement released claim: %v", err)
			}
			combinationCheckpoint(t, c, "replacement-preserved", map[string]any{"marker_matches_old_intent": marker, "physical": replacement, "remove_calls": nodes.removes, "old_intent": intent, "ledger_read": combinationLedger(t, c, episode.ID)})
			// Preserve the new test-owned entity off-path, then retry only the
			// original container's exact absence. No recursive delete is needed.
			parked := container + "-replacement-preserved"
			if err := os.Rename(container, parked); err != nil {
				t.Fatal(err)
			}
			err = NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
				removed, err := c.artifacts.CleanupRecoveryCopy(ctx, episode.ID, driver)
				if err != nil || removed.CopyRemovedAt.IsZero() {
					t.Fatalf("exact absence did not finally acknowledge old intent: %+v %v", removed, err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if nodes.removes != 1 || combinationJSON(t, combinationFiles(t, parked)) != combinationJSON(t, replacement) || combinationJSON(t, combinationFiles(t, p.Home.Path)) != combinationJSON(t, physical) {
				t.Fatal("absence retry deleted bytes or repeated RemoveRecovery")
			}
			if err := c.projects.Declare(t.Context(), []project.Project{p, other}); err != nil {
				t.Fatalf("exact absence acknowledgement kept old claim: %v", err)
			}
			combinationCheckpoint(t, c, "exact-absence-ack", map[string]any{"ledger_read": combinationLedger(t, c, episode.ID), "remove_calls": nodes.removes, "replacement_preserved": combinationFiles(t, parked), "canonical": physical, "claim": "released after exact absence only"})
		})
	}
}
