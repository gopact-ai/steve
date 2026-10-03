package turn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

type recoveryPreapplyFixture struct {
	c         *Coordinator
	project   project.Project
	episode   attempt.WorkspaceRecovery
	land      artifact.Landing
	nodes     *recoveryApplyFixture
	producers []attempt.Record
}

func blockedRecoveryPreapply(t *testing.T) recoveryPreapplyFixture {
	t.Helper()
	c, p, source, ws := sharedCopy(t)
	first := publishBoundRecoveryTurn(t, c, p, ws, "first-preapply", map[string]string{"incoming": "accepted incoming\n"})
	last := publishBoundRecoveryTurn(t, c, p, ws, "last-preapply", map[string]string{"other": "accepted other\n"})
	for name, body := range map[string]string{".gitignore": "incoming\n", "incoming": "uncaptured original\n"} {
		if err := os.WriteFile(filepath.Join(p.Home.Path, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	episode := captureBoundCopy(t, c, source)
	nodes := &recoveryApplyFixture{Nodes: artifact.LocalNodes{Dir: t.TempDir()}}
	c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, nodes)
	c.artifacts.SetExecution(c.executions)
	f := recoveryPreapplyFixture{c: c, project: p, episode: episode, nodes: nodes, producers: []attempt.Record{first, last}}
	land, err := retryRecoveryPreapply(t, f, t.Context())
	var conflict artifact.Conflict
	if !errors.As(err, &conflict) || land.State != artifact.LandApplyConflicted || !land.Unapplied || land.Round != 0 || land.Conflict != "" || land.ID != attempt.RecoveryLandingID(episode) {
		t.Fatalf("physical preapply conflict: %+v %v", land, err)
	}
	if nodes.applies != 0 {
		t.Fatalf("preapply conflict issued apply: %d", nodes.applies)
	}
	assertRecoveryRegistryIdle(t, c)
	f.land = land
	return f
}

func retryRecoveryPreapply(t *testing.T, f recoveryPreapplyFixture, ctx context.Context) (artifact.Landing, error) {
	t.Helper()
	var land artifact.Landing
	err := NewWorkspaceRecoveryControl(f.c).Drive(ctx, f.episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		var err error
		land, err = f.c.artifacts.LandRecoveryOnce(ctx, f.episode.ID, driver)
		return err
	})
	return land, err
}

func assertRecoveryRegistryIdle(t *testing.T, c *Coordinator) {
	t.Helper()
	if active := c.executions.Active(); len(active) != 0 {
		t.Errorf("landing left execution scopes active: %v", active)
	}
	release, err := c.executions.SealIdle()
	if err != nil {
		t.Errorf("landing prevented idle admission seal: %v", err)
		return
	}
	release()
}

func recoveryPendingBytes(t *testing.T, f recoveryPreapplyFixture) []byte {
	t.Helper()
	bindings, err := ledgerOf(t, f.c).Bindings(t.Context(), "pending-landing")
	if err != nil {
		t.Fatal(err)
	}
	return bindings[f.project.ID+"/"+f.episode.Head.Artifact]
}

func removePreapplyBlocker(t *testing.T, f recoveryPreapplyFixture) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.project.Home.Path, "incoming")); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryRetryCommitted(t *testing.T, f recoveryPreapplyFixture, land artifact.Landing, err error) {
	t.Helper()
	if err != nil || land.State != artifact.LandCommitted || land.ID != f.land.ID || land.Base != f.episode.Baseline.Artifact || land.StartedAt != f.land.StartedAt || land.Round != 1 || land.Unapplied || land.Error != "" || land.Committed == nil {
		t.Fatalf("cleared physical conflict did not commit the same newly admitted landing: %+v %v", land, err)
	}
	if raw := recoveryPendingBytes(t, f); len(raw) != 0 {
		t.Fatalf("committed preapply retry retained its obsolete Pending: %s", raw)
	}
	for name, body := range map[string]string{"incoming": "accepted incoming\n", "other": "accepted other\n", "original": "named base\n", ".gitignore": "incoming\n"} {
		if raw, err := os.ReadFile(filepath.Join(f.project.Home.Path, name)); err != nil || string(raw) != body {
			t.Fatalf("retry content %s: %q %v", name, raw, err)
		}
	}
	assertRecoveryRegistryIdle(t, f.c)
}

func TestRecoveryPreapplyConflictRetriesAfterPhysicalBlockerRemoval(t *testing.T) {
	f := blockedRecoveryPreapply(t)
	checks := 0
	f.nodes.before = func(req ops.Request) error {
		if req.Op == ops.PathState {
			checks++
		}
		return nil
	}
	stillBlocked, err := retryRecoveryPreapply(t, f, t.Context())
	var conflict artifact.Conflict
	if !errors.As(err, &conflict) || stillBlocked.ID != f.land.ID || !stillBlocked.Unapplied || stillBlocked.Round != 0 || checks == 0 {
		t.Fatalf("uncleared blocker was not physically rechecked under the same identity: %+v checks=%d err=%v", stillBlocked, checks, err)
	}
	if raw, err := os.ReadFile(filepath.Join(f.project.Home.Path, "incoming")); err != nil || string(raw) != "uncaptured original\n" || f.nodes.applies != 0 {
		t.Fatalf("retry wrote through the blocker: %q applies=%d err=%v", raw, f.nodes.applies, err)
	}
	assertRecoveryRegistryIdle(t, f.c)
	removePreapplyBlocker(t, f)
	checks = 0
	land, err := retryRecoveryPreapply(t, f, t.Context())
	assertRecoveryRetryCommitted(t, f, land, err)
	if checks == 0 || f.nodes.applies != 1 {
		t.Fatalf("successful retry skipped fresh preapply or repeated apply: checks=%d applies=%d", checks, f.nodes.applies)
	}
	if err := os.WriteFile(filepath.Join(f.project.Home.Path, "incoming"), []byte("later user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	replay, err := retryRecoveryPreapply(t, f, t.Context())
	if err != nil || replay.ID != land.ID || replay.Committed == nil || *replay.Committed != *land.Committed || f.nodes.applies != 1 {
		t.Fatalf("committed retry replayed physical work: %+v %v", replay, err)
	}
	if err := NewWorkspaceRecoveryControl(f.c).Drive(t.Context(), f.episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := f.c.artifacts.ReleaseRecovery(ctx, f.episode.ID, driver)
		return err
	}); err != nil {
		t.Fatalf("retried root could not release its exact result: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(f.project.Home.Path, "incoming")); err != nil || string(raw) != "later user edit\n" {
		t.Fatalf("committed replay overwrote later content: %q %v", raw, err)
	}
	assertRecoveryRegistryIdle(t, f.c)
}

func TestRecoveryPreapplyRetryKeepsPendingWhenAuthorityOrTransactionIsRefused(t *testing.T) {
	for _, mode := range []string{"owner", "maintenance", "first-source", "last-source", "cancelled", "transition-refused", "pending-delete-refused"} {
		t.Run(mode, func(t *testing.T) {
			f := blockedRecoveryPreapply(t)
			before := string(recoveryPendingBytes(t, f))
			removePreapplyBlocker(t, f)
			ctx := t.Context()
			owners := f.c.owners
			var trigger string
			switch mode {
			case "owner":
				var err error
				f.c.owners, err = newChannelOwners("different-owner", nil)
				if err != nil {
					t.Fatal(err)
				}
			case "maintenance":
				f.c.maintaining = true
			case "first-source", "last-source":
				index := 0
				if mode == "last-source" {
					index = 1
				}
				if _, err := f.c.tasks.SetAside(f.producers[index].TaskID, task.StateCancelled); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			case "transition-refused":
				trigger = "refuse_preapply_retry"
				forceStopTrigger(t, f.c, `CREATE TRIGGER refuse_preapply_retry BEFORE UPDATE ON operations WHEN NEW.kind='landing' AND OLD.state='apply-conflicted' AND NEW.state='proposed' BEGIN SELECT RAISE(ABORT,'preapply retry refused'); END`)
			case "pending-delete-refused":
				trigger = "refuse_preapply_pending"
				forceStopTrigger(t, f.c, `CREATE TRIGGER refuse_preapply_pending BEFORE DELETE ON bindings WHEN OLD.kind='pending-landing' BEGIN SELECT RAISE(ABORT,'preapply pending refused'); END`)
			}
			_, err := retryRecoveryPreapply(t, f, ctx)
			switch mode {
			case "first-source", "last-source":
				if !errors.Is(err, task.ErrExecutionStopped) {
					t.Fatalf("retry did not recheck every frozen source: %v", err)
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled retry: %v", err)
				}
			case "transition-refused", "pending-delete-refused":
				if err == nil || !strings.Contains(err.Error(), "refused") {
					t.Fatalf("retry did not reach its guarded transaction: %v", err)
				}
			default:
				if err == nil {
					t.Fatal("retry bypassed current owner or maintenance")
				}
			}
			if after := string(recoveryPendingBytes(t, f)); after != before {
				t.Fatalf("refused admission consumed Pending: before=%s after=%s", before, after)
			}
			op, found, err := ledgerOf(t, f.c).Operation(t.Context(), f.land.ID)
			if err != nil || !found || op.State != artifact.LandApplyConflicted {
				t.Fatalf("refused retry changed its durable state: %+v %v", op, err)
			}
			if _, err := os.Stat(filepath.Join(f.project.Home.Path, "incoming")); !os.IsNotExist(err) || f.nodes.applies != 0 {
				t.Fatalf("refused retry wrote original: applies=%d err=%v", f.nodes.applies, err)
			}
			assertRecoveryRegistryIdle(t, f.c)
			if mode == "first-source" || mode == "last-source" {
				return
			}
			f.c.owners, f.c.maintaining = owners, false
			if trigger != "" {
				if err := ledgerOf(t, f.c).Update(t.Context(), func(tx *ledger.Tx) error {
					_, err := tx.Exec("DROP TRIGGER " + trigger)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			land, err := retryRecoveryPreapply(t, f, t.Context())
			assertRecoveryRetryCommitted(t, f, land, err)
		})
	}
}

func TestRecoveryPreapplyRetryRequiresExactPendingLineage(t *testing.T) {
	for _, field := range []string{"root", "head-version", "canonical", "marked"} {
		t.Run(field, func(t *testing.T) {
			f := blockedRecoveryPreapply(t)
			removePreapplyBlocker(t, f)
			var item artifact.Pending
			if err := json.Unmarshal(recoveryPendingBytes(t, f), &item); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "root":
				item.Resolution.Root = "different-root"
			case "head-version":
				item.Resolution.HeadVersion++
			case "canonical":
				item.Resolution.Canonical = f.episode.Head.Artifact
				item.Blocked.Canonical = item.Resolution.Canonical
			case "marked":
				item.Resolution.Marked = f.episode.Head.Artifact
				item.Blocked.Marked = item.Resolution.Marked
			}
			if err := ledgerOf(t, f.c).Update(t.Context(), func(tx *ledger.Tx) error {
				return tx.PutBinding("pending-landing", f.project.ID+"/"+f.episode.Head.Artifact, item)
			}); err != nil {
				t.Fatal(err)
			}
			before := string(recoveryPendingBytes(t, f))
			_, err := retryRecoveryPreapply(t, f, t.Context())
			if !errors.Is(err, artifact.ErrNotBlocked) {
				t.Fatalf("retry trusted a different %s lineage: %v", field, err)
			}
			if after := string(recoveryPendingBytes(t, f)); before != after || f.nodes.applies != 0 {
				t.Fatalf("retry consumed another lineage: applies=%d before=%s after=%s", f.nodes.applies, before, after)
			}
			assertRecoveryRegistryIdle(t, f.c)
		})
	}
}

func TestRecoveryPreapplyRetryRechecksSourcesBeforeApplying(t *testing.T) {
	f := blockedRecoveryPreapply(t)
	removePreapplyBlocker(t, f)
	revoked := false
	f.nodes.before = func(req ops.Request) error {
		if req.Op == ops.PathState && !revoked {
			revoked = true
			_, err := f.c.tasks.SetAside(f.producers[0].TaskID, task.StateCancelled)
			return err
		}
		return nil
	}
	_, err := retryRecoveryPreapply(t, f, t.Context())
	if !errors.Is(err, task.ErrExecutionStopped) || !revoked || f.nodes.applies != 0 {
		t.Fatalf("retry missed revocation after fresh preapply: revoked=%v applies=%d err=%v", revoked, f.nodes.applies, err)
	}
	if _, err := os.Stat(filepath.Join(f.project.Home.Path, "incoming")); !os.IsNotExist(err) {
		t.Fatalf("late-revoked retry wrote original: %v", err)
	}
	assertRecoveryRegistryIdle(t, f.c)
}

func TestRecoveryResolverPreapplyRetryClosesEveryAcquiredScope(t *testing.T) {
	for _, registered := range []bool{true, false} {
		for _, cancelResolver := range []bool{true, false} {
			name := "registered"
			if !registered {
				name = "without-registry"
			}
			if cancelResolver {
				name += "/cancelled"
			} else {
				name += "/authorized"
			}
			t.Run(name, func(t *testing.T) {
				c, p, _, stuck := recoveryConflictFixture(t)
				source, id := acceptedResolverFixture(t, c, p, stuck, "retry-resolver", "resolved retry\n")
				nodes := &recoveryApplyFixture{Nodes: artifact.LocalNodes{Dir: t.TempDir()}}
				c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, nodes)
				if registered {
					c.artifacts.SetExecution(c.executions)
				}
				c.artifacts.SetRecoveryResolutionDriver(NewWorkspaceRecoveryControl(c).Drive)
				refused := errors.New("original snapshot refused")
				nodes.before = func(req ops.Request) error {
					if req.Op == ops.Snapshot && req.WorkTree == p.Home.Path {
						if registered && len(c.executions.Active()) != 2 {
							t.Errorf("snapshot did not own both original and resolver scopes: %v", c.executions.Active())
						}
						return refused
					}
					return nil
				}
				land, err := c.artifacts.LandRecoveryResolutionOnce(t.Context(), *stuck.Resolution, id, source)
				if !errors.Is(err, refused) || land.State != artifact.LandLocked || land.Round != 0 {
					t.Fatalf("preapply interruption: %+v %v", land, err)
				}
				assertRecoveryRegistryIdle(t, c)
				if cancelResolver {
					if _, err := c.tasks.SetAside(source.Execution.TaskID, task.StateCancelled); err != nil {
						t.Fatal(err)
					}
				}
				nodes.before = nil
				retry, err := c.artifacts.LandRecoveryResolutionOnce(t.Context(), *stuck.Resolution, id, source)
				if cancelResolver {
					if !errors.Is(err, task.ErrExecutionStopped) || nodes.applies != 0 {
						t.Fatalf("revoked resolver retry: %+v applies=%d err=%v", retry, nodes.applies, err)
					}
					if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "original")); err != nil || string(raw) != "original side\n" {
						t.Fatalf("revoked resolver wrote original: %q %v", raw, err)
					}
				} else if err != nil || retry.State != artifact.LandCommitted || retry.ID != land.ID || nodes.applies != 1 {
					t.Fatalf("authorized resolver retry: %+v applies=%d err=%v", retry, nodes.applies, err)
				}
				assertRecoveryRegistryIdle(t, c)
			})
		}
	}
}
