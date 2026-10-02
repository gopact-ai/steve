package turn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type residualCapturer interface {
	CaptureRecoveryResidual(context.Context, string, ledger.Lease) (attempt.WorkspaceRecovery, error)
}

type observedResidual struct {
	Artifact string   `json:"artifact"`
	Evidence string   `json:"evidence"`
	Nested   []string `json:"excluded_nested"`
}

func residualOf(t *testing.T, r attempt.WorkspaceRecovery) *observedResidual {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Residual *observedResidual `json:"residual"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Residual
}

func drainWithoutCopy(t *testing.T) (*Coordinator, project.Project, attempt.WorkspaceRecovery) {
	t.Helper()
	c, p, original, _ := recoveryCopyFixture(t, true)
	original, err := NewAbandonControl(c).AbandonAttempt(t.Context(), original.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), original.ID); err != nil {
		t.Fatal(err)
	}
	original = retireOriginalRecoverySource(t, c, original)
	var episode attempt.WorkspaceRecovery
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), original.Abandoned.WorkspaceRecoveryID, func(ctx context.Context, driver ledger.Lease) error {
		enrollStoppedOriginals(t, ctx, c, original.Abandoned.WorkspaceRecoveryID, driver)
		var err error
		episode, err = c.attempts.BeginRecoveryDrain(ctx, original.Abandoned.WorkspaceRecoveryID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c, p, episode
}

func TestRecoveryResidualCaptureKeepsCurrentOriginalContentAndItsName(t *testing.T) {
	c, p, episode := drainWithoutCopy(t)
	capture, ok := any(c.artifacts).(residualCapturer)
	if !ok {
		t.Fatal("recovery has no exact non-destructive residual capture consumer")
	}
	for name, body := range map[string]string{"original": "residual current state\n", "inputs/owned": "ordinary inputs\n", ".gitignore": "ignored\n", "ignored": "keep ignored bytes\n", "nested/.git/HEAD": "ref: refs/heads/example\n", "nested/owned": "keep nested bytes\n"} {
		full := filepath.Join(p.Home.Path, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before, found, err := c.artifacts.Resolve(t.Context(), artifact.CanonicalRef(p.ID))
	if err != nil || !found {
		t.Fatalf("named base: %+v %v", before, err)
	}
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		captured, err := capture.CaptureRecoveryResidual(ctx, episode.ID, driver)
		if err != nil {
			return err
		}
		residual := residualOf(t, captured)
		if captured.Phase != "landing" || captured.Workspace.Path != "" || residual == nil || residual.Artifact == episode.Baseline.Artifact || residual.Evidence == "" || len(residual.Nested) == 0 {
			t.Fatalf("capture lost fixed/no-copy residual identity: %+v %+v", captured, residual)
		}
		if _, found, err := c.artifacts.Manifest(ctx, residual.Artifact); err != nil || !found {
			t.Fatalf("residual was not accepted by artifact owner: %v %v", found, err)
		}
		if err := os.WriteFile(filepath.Join(p.Home.Path, "later"), []byte("after capture\n"), 0600); err != nil {
			return err
		}
		replay, err := capture.CaptureRecoveryResidual(ctx, episode.ID, driver)
		if err != nil {
			return err
		}
		if got := residualOf(t, replay); got == nil || got.Artifact != residual.Artifact {
			t.Fatalf("replay replaced accepted R with a later observation: %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, found, err := c.artifacts.Resolve(t.Context(), artifact.CanonicalRef(p.ID))
	if err != nil || !found || after != before {
		t.Fatalf("capturing R moved canonical or restored B: %+v %+v %v", before, after, err)
	}
	for name, body := range map[string]string{"original": "residual current state\n", "inputs/owned": "ordinary inputs\n", "ignored": "keep ignored bytes\n", "nested/.git/HEAD": "ref: refs/heads/example\n", "nested/owned": "keep nested bytes\n", "later": "after capture\n"} {
		if raw, err := os.ReadFile(filepath.Join(p.Home.Path, name)); err != nil || string(raw) != body {
			t.Fatalf("capture modified original path %s: %q %v", name, raw, err)
		}
	}
	if _, _, err := c.artifacts.SnapshotCanonical(t.Context(), p, "", "fixture", "ordinary capture"); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("internal permit leaked to ordinary snapshot: %v", err)
	}
}

func TestRecoveryResidualAndNewArtifactAreAcceptedInOneTransaction(t *testing.T) {
	c, p, episode := drainWithoutCopy(t)
	capture, ok := any(c.artifacts).(residualCapturer)
	if !ok {
		t.Fatal("recovery has no atomic residual acceptance consumer")
	}
	if err := os.WriteFile(filepath.Join(p.Home.Path, "residual"), []byte("must persist together\n"), 0600); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error {
			return tx.QueryRow(`SELECT count(*) FROM bindings WHERE kind='artifact'`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_residual BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND json_extract(NEW.data,'$.residual') IS NOT NULL BEGIN SELECT RAISE(ABORT,'residual refused'); END`)
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		if _, err := capture.CaptureRecoveryResidual(ctx, episode.ID, driver); err == nil {
			t.Fatal("residual refusal became capture success")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || residualOf(t, current) != nil || current.Phase == "landing" || count() != before {
		t.Fatalf("rejected R left an accepted artifact or released phase: %+v artifacts=%d/%d err=%v", current, count(), before, err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_residual"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := capture.CaptureRecoveryResidual(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if count() != before+1 {
		t.Fatal("retry did not accept precisely one captured artifact")
	}
}

func TestRecoveryPreparedWithoutNativeIdentityCannotCaptureOrLoseItsObligation(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	writer, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "unbound-native"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkRecoveryWriting(t.Context(), writer.ID); err != nil {
		t.Fatal(err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || len(episode.NativeRetirements) != 1 || episode.NativeRetirements[0].Session != "" || episode.NativeRetirements[0].Binding.AttemptID != writer.ID {
		t.Fatalf("native preparation began before durable exact open obligation: %+v %v", episode, err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		if _, err := c.attempts.Advance(t.Context(), writer.ID, phase, "fixture", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), writer.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	writer, err = c.attempts.Get(t.Context(), writer.ID)
	if err != nil {
		t.Fatal(err)
	}
	completion, _, err := c.completion(t.Context(), writer, Result{Text: "unchanged but no native receipt"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FinishCompletion(t.Context(), writer.ID, "fixture", completion); err != nil {
		t.Fatal(err)
	}
	source = retireOriginalRecoverySource(t, c, source)
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		enrollStoppedOriginals(t, ctx, c, episode.ID, driver)
		if _, err := c.attempts.BeginRecoveryDrain(ctx, episode.ID, driver); err != nil {
			return err
		}
		if _, err := c.artifacts.CaptureRecoveryResidual(ctx, episode.ID, driver); !errors.Is(err, attempt.ErrStopConfirmationRequired) {
			t.Fatalf("settled/empty native identity became process exit: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || current.Phase != "draining" || current.Residual != nil || len(current.NativeRetirements) != 2 || current.NativeRetirements[0].Proof != nil {
		t.Fatalf("unbound preparation was lost or captured: %+v %v", current, err)
	}
}

func TestRecoveryCaptureRequiresExactDriverAndTheCurrentOwner(t *testing.T) {
	c, p, episode := drainWithoutCopy(t)
	if err := os.WriteFile(filepath.Join(p.Home.Path, "current"), []byte("preserve\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.artifacts.CaptureRecoveryResidual(t.Context(), episode.ID, ledger.Lease{Key: "workspace-recovery-driver:another"}); err == nil {
		t.Fatal("arbitrary driver minted residual permission")
	}
	called := false
	c.maintaining = true
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(context.Context, ledger.Lease) error { called = true; return nil }); err == nil || called {
		t.Fatal("maintenance authorized recovery I/O")
	}
	c.maintaining = false
	owners, err := newChannelOwners("different-owner", nil)
	c.owners = owners
	if err != nil {
		t.Fatal(err)
	}
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(context.Context, ledger.Lease) error { called = true; return nil }); err == nil || called {
		t.Fatal("historical requester became current owner permission")
	}
	current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || current.Residual != nil || current.Phase != "draining" {
		t.Fatalf("refused owner/driver accepted R: %+v %v", current, err)
	}
}

type recoveryLander interface {
	LandRecoveryOnce(context.Context, string, ledger.Lease) (artifact.Landing, error)
}

func publishBoundRecoveryTurn(t *testing.T, c *Coordinator, p project.Project, ws project.Workspace, id string, files map[string]string) attempt.Record {
	t.Helper()
	r, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, id))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.tasks.BindAttempt(*r.Execution, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	r, err = c.attempts.Advance(t.Context(), r.ID, attempt.Prepared, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err = c.attempts.RecordSession(t.Context(), r.ID, "fixture", "ns_"+id)
	if err != nil {
		t.Fatal(err)
	}
	r, err = c.attempts.Advance(t.Context(), r.ID, attempt.Running, "fixture", func(r *attempt.Record) { r.NativeContext = "native-" + id })
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		full := filepath.Join(ws.Path, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	r, err = c.attempts.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	completion, _, err := c.completion(t.Context(), r, Result{Text: "accepted"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err = c.attempts.FinishCompletion(t.Context(), r.ID, "fixture", completion)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func captureBoundCopy(t *testing.T, c *Coordinator, source attempt.Record) attempt.WorkspaceRecovery {
	t.Helper()
	source = retireOriginalRecoverySource(t, c, source)
	var result attempt.WorkspaceRecovery
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), source.Abandoned.WorkspaceRecoveryID, func(ctx context.Context, driver ledger.Lease) error {
		enrollStoppedOriginals(t, ctx, c, source.Abandoned.WorkspaceRecoveryID, driver)
		episode, err := c.attempts.BeginRecoveryDrain(ctx, source.Abandoned.WorkspaceRecoveryID, driver)
		if err != nil {
			return err
		}
		for _, n := range episode.NativeRetirements {
			if !n.Copy {
				continue
			}
			proof := attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: nodewire.SessionState{ID: n.Session, ContextID: n.Context, Harness: n.Harness, Binding: n.Binding, State: nodewire.SessionClosed, ProcessStopped: true, InputAccepted: 1, Command: &nodewire.SessionCommand{ID: n.Command, InputSequence: 1, State: nodewire.SessionCommandCompleted, Settled: true, ProcessStopped: true}}}
			if err := c.attempts.AcceptRecoveryNativeStop(ctx, episode, n, driver, proof); err != nil {
				return err
			}
			if err := c.store.RetireRecoverySession(ctx, n.Binding.NodeID, n.Harness, n.Session, n.Binding.AttemptID, time.Now().UTC().Format(time.RFC3339Nano), func(tx *ledger.Tx) error { return c.attempts.RetireRecoveryNativeTx(tx, episode.ID, n, driver) }); err != nil {
				return err
			}
		}
		result, err = c.artifacts.CaptureRecoveryResidual(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRecoveryLandingUsesFixedBaselineAndTheActualCurrentOriginal(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	first := publishBoundRecoveryTurn(t, c, p, ws, "first-fixed", map[string]string{"original": "copy delta\n"})
	publishBoundRecoveryTurn(t, c, p, ws, "second-fixed", map[string]string{"copy-only": "second delta\n"})
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		ref, _, err := tx.Name(artifact.CanonicalRef(p.ID))
		if err != nil {
			return err
		}
		_, err = tx.CompareAndSetName(ref.Name, ref.Version, first.Result.Artifact)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	episode := captureBoundCopy(t, c, source)
	if err := os.WriteFile(filepath.Join(p.Home.Path, "after-capture"), []byte("external later edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lander, ok := any(c.artifacts).(recoveryLander)
	if !ok {
		t.Fatal("recovery has no fixed-baseline all-source stable landing consumer")
	}
	var landed artifact.Landing
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		var err error
		landed, err = lander.LandRecoveryOnce(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if landed.State != artifact.LandCommitted || landed.Base != episode.Baseline.Artifact {
		t.Fatalf("landing drifted to a current canonical ancestor: %+v", landed)
	}
	for name, body := range map[string]string{"original": "copy delta\n", "copy-only": "second delta\n", "after-capture": "external later edit\n"} {
		if raw, err := os.ReadFile(filepath.Join(p.Home.Path, name)); err != nil || string(raw) != body {
			t.Fatalf("fixed B/R'/C lost %s: %q %v", name, raw, err)
		}
	}
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		replay, err := lander.LandRecoveryOnce(ctx, episode.ID, driver)
		if replay.ID != landed.ID || replay.State != artifact.LandCommitted {
			t.Fatalf("replay replaced stable landing: %+v", replay)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryLandingChecksEveryAcceptedProducerBeforeAdmission(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	first := publishBoundRecoveryTurn(t, c, p, ws, "first-authority", map[string]string{"first": "first\n"})
	publishBoundRecoveryTurn(t, c, p, ws, "last-authority", map[string]string{"last": "last\n"})
	episode := captureBoundCopy(t, c, source)
	lander, ok := any(c.artifacts).(recoveryLander)
	if !ok {
		t.Fatal("recovery has no complete-source landing authorization consumer")
	}
	if _, err := c.tasks.SetAside(first.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := lander.LandRecoveryOnce(ctx, episode.ID, driver)
		if !errors.Is(err, task.ErrExecutionStopped) {
			t.Fatalf("last token replaced first producer authorization: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "last"} {
		if _, err := os.Stat(filepath.Join(p.Home.Path, name)); !os.IsNotExist(err) {
			t.Fatalf("revoked source changed original path %s: %v", name, err)
		}
	}
}

func TestRecoveryLandingDoesNotOverwriteAnIgnoredIncomingPath(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	publishBoundRecoveryTurn(t, c, p, ws, "ignored-authority", map[string]string{"incoming": "copy data\n"})
	for name, body := range map[string]string{".gitignore": "incoming\n", "incoming": "original ignored data\n"} {
		if err := os.WriteFile(filepath.Join(p.Home.Path, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	episode := captureBoundCopy(t, c, source)
	lander, ok := any(c.artifacts).(recoveryLander)
	if !ok {
		t.Fatal("recovery has no physical preapply protection consumer")
	}
	nodes := &recoveryApplyFixture{Nodes: artifact.LocalNodes{Dir: t.TempDir()}}
	c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, nodes)
	lander = c.artifacts
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		land, err := lander.LandRecoveryOnce(ctx, episode.ID, driver)
		var conflict artifact.Conflict
		if !errors.As(err, &conflict) || land.State != artifact.LandApplyConflicted {
			t.Fatalf("uncaptured ignored entity was not conservatively refused: %+v %v", land, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if nodes.applies != 0 {
		t.Fatalf("uncaptured entity reached applying before refusal: %d", nodes.applies)
	}
	if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "incoming")); err != nil || string(raw) != "original ignored data\n" {
		t.Fatalf("landing overwrote ignored original bytes: %q %v", raw, err)
	}
}

type recoveryApplyFixture struct {
	artifact.Nodes
	before    func(ops.Request) error
	failApply bool
	applies   int
}

func (n *recoveryApplyFixture) Artifact(ctx context.Context, node string, req ops.Request) (ops.Result, error) {
	if n.before != nil {
		if err := n.before(req); err != nil {
			return ops.Result{}, err
		}
	}
	if req.Op == ops.Apply {
		n.applies++
		if n.failApply {
			_, err := n.Nodes.Artifact(ctx, node, ops.Request{Op: ops.WritePath, Repo: req.Repo, WorkTree: req.WorkTree, Commit: req.Commit, Path: "first-wal"})
			if err != nil {
				return ops.Result{}, err
			}
			if err := os.WriteFile(filepath.Join(req.WorkTree, "later-wal"), []byte("external after partial apply\n"), 0600); err != nil {
				return ops.Result{}, err
			}
			return ops.Result{}, errors.New("fixture partial apply")
		}
	}
	return n.Nodes.Artifact(ctx, node, req)
}

func TestRecoveryLandingReplaysAdmittedWALAndRecordsActualCanonical(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	producer := publishBoundRecoveryTurn(t, c, p, ws, "wal-authority", map[string]string{"first-wal": "first\n", "second-wal": "second\n"})
	episode := captureBoundCopy(t, c, source)
	local := artifact.LocalNodes{Dir: t.TempDir()}
	// A fresh isolated typed node imports the real bundle prerequisites.
	// Its transport operates only on this fixture's original directory.
	nodes := &recoveryApplyFixture{Nodes: local}
	c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, nodes)
	nodes.failApply = true
	nodes.before = func(req ops.Request) error {
		if req.Op == ops.Apply {
			_, err := c.tasks.SetAside(producer.TaskID, task.StateCancelled)
			return err
		}
		return nil
	}
	var land artifact.Landing
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		var err error
		land, err = c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if land.State != artifact.LandCommitted || land.Committed == nil || land.Committed.Artifact == land.Merged {
		t.Fatalf("WAL actual canonical receipt was guessed from Merged: %+v", land)
	}
	for name, body := range map[string]string{"first-wal": "first\n", "second-wal": "second\n", "later-wal": "external after partial apply\n"} {
		if raw, err := os.ReadFile(filepath.Join(p.Home.Path, name)); err != nil || string(raw) != body {
			t.Fatalf("WAL recovery lost %s: %q %v", name, raw, err)
		}
	}
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		again, err := c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver)
		if again.ID != land.ID || again.Committed == nil || *again.Committed != *land.Committed {
			t.Fatalf("committed retry guessed a new result: %+v", again)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if nodes.applies != 1 {
		t.Fatalf("committed WAL replay applied again: %d", nodes.applies)
	}
}

func TestRecoveryLandingRechecksRevocationBetweenPreflightAndApplying(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	producer := publishBoundRecoveryTurn(t, c, p, ws, "revoked-at-admission", map[string]string{"incoming": "do not apply\n"})
	episode := captureBoundCopy(t, c, source)
	nodes := &recoveryApplyFixture{Nodes: artifact.LocalNodes{Dir: t.TempDir()}}
	c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, nodes)
	once := false
	nodes.before = func(req ops.Request) error {
		if req.Op == ops.PathState && !once {
			once = true
			_, err := c.tasks.SetAside(producer.TaskID, task.StateCancelled)
			return err
		}
		return nil
	}
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver)
		if !errors.Is(err, task.ErrExecutionStopped) {
			t.Fatalf("revocation between preflight/applying was ignored: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !once || nodes.applies != 0 {
		t.Fatalf("late revocation reached native apply: revoked=%v calls=%d", once, nodes.applies)
	}
	if _, err := os.Stat(filepath.Join(p.Home.Path, "incoming")); !os.IsNotExist(err) {
		t.Fatalf("revoked new admission wrote original: %v", err)
	}
}

type recoveryReleaser interface {
	ReleaseRecovery(context.Context, string, ledger.Lease) (attempt.WorkspaceRecovery, error)
}

type resolutionDriverInstaller interface {
	SetRecoveryResolutionDriver(func(context.Context, string, func(context.Context, ledger.Lease) error) error)
}

func TestRecoveryResultAndOriginalHoldReleaseCommitAtomically(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	publishBoundRecoveryTurn(t, c, p, ws, "release-authority", map[string]string{"landed": "accepted\n"})
	episode := captureBoundCopy(t, c, source)
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	release, ok := any(c.artifacts).(recoveryReleaser)
	if !ok {
		t.Fatal("recovery has no canonical-result/hold atomic release consumer")
	}
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_released BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND NEW.state='released' BEGIN SELECT RAISE(ABORT,'release refused'); END`)
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		if _, err := release.ReleaseRecovery(ctx, episode.ID, driver); err == nil {
			t.Fatal("refused release became success")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || current.Phase != "landing" {
		t.Fatalf("refused result released episode: %+v %v", current, err)
	}
	if err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error { return attempt.RecoveryHoldTx(tx, p.Home.Node, p.Home.Path) }); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("result refusal lost original hold: %v", err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_released"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		r, err := release.ReleaseRecovery(ctx, episode.ID, driver)
		if err == nil && r.Phase != "released" {
			t.Fatalf("result was not atomically released: %+v", r)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.attempts.RecoveryForProject(t.Context(), p.ID); err != nil || found {
		t.Fatalf("released episode still routes new work to its copy: %v %v", found, err)
	}
	if err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error { return attempt.RecoveryHoldTx(tx, p.Home.Node, p.Home.Path) }); err != nil {
		t.Fatalf("released original remains held: %v", err)
	}
	if _, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "old-copy-after-release")); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("released copy admitted another writer: %v", err)
	}
}

func TestRecoveryConflictManualResolutionKeepsExactRootAndConsumesPendingOnce(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	publishBoundRecoveryTurn(t, c, p, ws, "conflict-authority", map[string]string{"original": "copy side\n"})
	if err := os.WriteFile(filepath.Join(p.Home.Path, "original"), []byte("original side\n"), 0600); err != nil {
		t.Fatal(err)
	}
	episode := captureBoundCopy(t, c, source)
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver)
		var conflict artifact.Conflict
		if !errors.As(err, &conflict) {
			t.Fatalf("root conflict was not preserved: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stuck, err := c.artifacts.Stuck(t.Context(), p.ID)
	if err != nil || len(stuck) != 1 || stuck[0].Marked == "" {
		t.Fatalf("recovery conflict has no visible exact-root consumer: %+v %v", stuck, err)
	}
	install, ok := any(c.artifacts).(resolutionDriverInstaller)
	if !ok {
		t.Fatal("recovery resolution has no actual owner/driver sink")
	}
	install.SetRecoveryResolutionDriver(NewWorkspaceRecoveryControl(c).Drive)
	land, err := c.artifacts.ResolveByHand(t.Context(), p, stuck[0], []artifact.Edit{{Path: "original", Text: "resolved by owner\n"}}, "console")
	if err != nil || land.State != artifact.LandCommitted || land.Committed == nil {
		t.Fatalf("same-root manual resolution did not commit: %+v %v", land, err)
	}
	release, ok := any(c.artifacts).(recoveryReleaser)
	if !ok {
		t.Fatal("resolved recovery has no atomic completion consumer")
	}
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := release.ReleaseRecovery(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := c.artifacts.Stuck(t.Context(), p.ID); err != nil || len(pending) != 0 {
		t.Fatalf("released root was not exactly consumed: %+v %v", pending, err)
	}
	if _, err := c.artifacts.LandPending(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "original")); err != nil || string(raw) != "resolved by owner\n" {
		t.Fatalf("ordinary pending re-landed old C: %q %v", raw, err)
	}
}

func recoveryConflictFixture(t *testing.T) (*Coordinator, project.Project, attempt.WorkspaceRecovery, artifact.Stuck) {
	t.Helper()
	c, p, source, ws := sharedCopy(t)
	publishBoundRecoveryTurn(t, c, p, ws, "root-conflict", map[string]string{"original": "copy side\n"})
	for name, body := range map[string]string{"original": "original side\n", "inputs/ordinary": "keep ordinary input\n"} {
		full := filepath.Join(p.Home.Path, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	episode := captureBoundCopy(t, c, source)
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver)
		var conflict artifact.Conflict
		if !errors.As(err, &conflict) {
			t.Fatalf("root merge: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stuck, err := c.artifacts.Stuck(t.Context(), p.ID)
	if err != nil || len(stuck) != 1 || stuck[0].Resolution == nil {
		t.Fatalf("missing exact conflict reference: %+v %v", stuck, err)
	}
	c.artifacts.SetRecoveryResolutionDriver(NewWorkspaceRecoveryControl(c).Drive)
	return c, p, episode, stuck[0]
}

func acceptedResolverFixture(t *testing.T, c *Coordinator, p project.Project, stuck artifact.Stuck, id, body string) (artifact.Source, string) {
	t.Helper()
	ws, err := c.artifacts.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Base: stuck.Marked, Owner: id})
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := c.tasks.Create(task.Task{Channel: "console:" + id, Transport: "console", Member: "worker", ProjectID: p.ID, Workspace: ws.Path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.Begin(tracked.ID, "worker", ws.Node, ""); err != nil {
		t.Fatal(err)
	}
	token, err := c.tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.attempts.Open(t.Context(), attempt.Spec{ID: id, Kind: attempt.KindStep, TaskID: tracked.ID, TurnID: id, Execution: &token, Project: p.ID, Node: ws.Node, Harness: "mock", Agent: "worker", Scope: attempt.ScopePathSet, Workspace: ws, Base: stuck.Marked})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		if _, err := c.attempts.Advance(t.Context(), r.ID, phase, "fixture", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "original"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	source := artifact.Source{Execution: &token, AttemptID: r.ID}
	m, _, err := c.artifacts.PublishRecoveryResolution(t.Context(), *stuck.Resolution, ws, stuck.Marked, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Snapshotted, attempt.Published, attempt.Durable, attempt.Verifying, attempt.BindReady} {
		if _, err := c.attempts.Advance(t.Context(), r.ID, phase, "fixture", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.attempts.Complete(t.Context(), r.ID, "fixture", attempt.Completion{Result: attempt.Result{Artifact: m.ID}}); err != nil {
		t.Fatal(err)
	}
	return source, m.ID
}

func TestRecoveryResolverSinkRechecksOwnerMaintenanceAndExactConflictAfterPublication(t *testing.T) {
	for _, mode := range []string{"owner", "maintenance", "wrong-root", "source-stopped", "valid"} {
		t.Run(mode, func(t *testing.T) {
			c, p, episode, stuck := recoveryConflictFixture(t)
			source, id := acceptedResolverFixture(t, c, p, stuck, "resolver", "resolved agent text\n")
			ref := *stuck.Resolution
			switch mode {
			case "owner":
				var err error
				c.owners, err = newChannelOwners("different-owner", nil)
				if err != nil {
					t.Fatal(err)
				}
			case "maintenance":
				c.maintaining = true
			case "wrong-root":
				ref.Root = "another-root"
			case "source-stopped":
				if _, err := c.tasks.SetAside(source.Execution.TaskID, task.StateCancelled); err != nil {
					t.Fatal(err)
				}
			}
			land, err := c.artifacts.LandRecoveryResolutionOnce(t.Context(), ref, id, source)
			if mode != "valid" {
				if err == nil {
					t.Fatalf("late resolver authority %s was ignored: %+v", mode, land)
				}
				if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "original")); err != nil || string(raw) != "original side\n" {
					t.Fatalf("rejected sink changed original: %q %v", raw, err)
				}
				return
			}
			if err != nil || land.State != artifact.LandCommitted {
				t.Fatalf("authorized same-root resolver sink: %+v %v", land, err)
			}
			if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
				_, err := c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"original": "resolved agent text\n", "inputs/ordinary": "keep ordinary input\n"} {
				if raw, err := os.ReadFile(filepath.Join(p.Home.Path, name)); err != nil || string(raw) != body {
					t.Fatalf("resolver dropped %s: %q %v", name, raw, err)
				}
			}
		})
	}
}

func TestRecoveryOldResolverSinkCannotRebindAfterAnotherResolutionWins(t *testing.T) {
	c, p, episode, stuck := recoveryConflictFixture(t)
	source, id := acceptedResolverFixture(t, c, p, stuck, "old-resolver", "old proposed resolution\n")
	land, err := c.artifacts.ResolveByHand(t.Context(), p, stuck, []artifact.Edit{{Path: "original", Text: "new owner resolution\n"}}, "console")
	if err != nil || land.State != artifact.LandCommitted {
		t.Fatalf("new resolution: %+v %v", land, err)
	}
	if _, err := c.artifacts.LandRecoveryResolutionOnce(t.Context(), *stuck.Resolution, id, source); err == nil {
		t.Fatal("old persisted sink rebound to the winning result")
	}
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_winner_release BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND NEW.state='released' BEGIN SELECT RAISE(ABORT,'winner release refused'); END`)
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
		if err == nil {
			t.Fatal("release refusal ignored")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_winner_release"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.artifacts.LandPending(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "original")); err != nil || string(raw) != "new owner resolution\n" {
		t.Fatalf("old C returned after committed winner/release replay: %q %v", raw, err)
	}
}

func TestRecoveryFinalizingInputWaitsAndRefreshesOnlyBeforeCanonicalAdmission(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	publishBoundRecoveryTurn(t, c, p, ws, "queued-copy-source", map[string]string{"incoming": "accepted\n"})
	episode := captureBoundCopy(t, c, source)
	tracked, err := c.tasks.Create(task.Task{Channel: "console:queued", Transport: "console", Member: "worker", ProjectID: p.ID, Workspace: ws.Path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.BeginTurn(tracked.ID, "worker", ws.Node, task.TurnInput{TurnID: "queued-turn", Address: channel.Address{Conversation: "console:queued", Channel: "console", Message: "queued-turn"}}); err != nil {
		t.Fatal(err)
	}
	scope, err := c.executions.Begin(t.Context(), execution.Key{TaskID: tracked.ID, InstanceID: "queued-turn"})
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Finish(nil)
	selected := agent.Agent{ID: "worker", Node: ws.Node, Harness: "mock"}
	waiting := make(chan struct{})
	var once sync.Once
	req := Request{ConversationID: "console:queued", MessageID: "queued-turn", SenderOpenID: "owner", OnStage: func(view.Stage) { once.Do(func() { close(waiting) }) }}
	spec, _, err := c.turnSpec(scope.Context(), req, selected, tracked.ID, project.Binding{ProjectID: p.ID}, ws)
	if err != nil {
		t.Fatal(err)
	}
	taskTurn := &chatTurn{c: c, req: req, selected: selected, tracked: tracked.ID, binding: project.Binding{ProjectID: p.ID}, workspace: ws, clock: newTurnClock(), spent: &turnSpend{resetIdle: func() {}}}
	admission := recoveryAdmissionAttempts{waitingAttempts: waitingAttempts{Attempts: c.attempts, passes: func(error) bool { return false }}, turn: taskTurn}
	done := make(chan attempt.Record, 1)
	failed := make(chan error, 1)
	go func() {
		r, err := admission.Open(scope.Context(), spec)
		if err != nil {
			failed <- err
		} else {
			done <- r
		}
	}()
	select {
	case <-waiting:
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("finalizing input was not retained as waiting")
	}
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		if _, err := c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver); err != nil {
			return err
		}
		_, err := c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		t.Fatal(err)
	case admitted := <-done:
		if admitted.Workspace.Kind != project.KindCanonical || admitted.Workspace.Path != p.Home.Path || admitted.WorkspaceRecovery != nil {
			t.Fatalf("queued old Spec resumed in the retired copy: %+v", admitted.Spec)
		}
		if taskTurn.saved.UpstreamID != "" {
			t.Fatal("native cwd was hot-switched instead of freshly opened")
		}
		if _, err := c.attempts.FailWith(t.Context(), admitted.ID, "fixture", "no prompt in fixture", nil); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("released input did not refresh canonical admission")
	}
}

func TestRecoveryResolverAdmittedWALSurvivesTaskCancelAndOwnerChangeWithoutNewAuthority(t *testing.T) {
	c, p, episode, stuck := recoveryConflictFixture(t)
	source, id := acceptedResolverFixture(t, c, p, stuck, "cancel-after-applying", "accepted resolver text\n")
	nodes := &recoveryApplyFixture{Nodes: artifact.LocalNodes{Dir: t.TempDir()}}
	c.artifacts = artifact.New(c.artifacts.Dir, ledgerOf(t, c), c.projects, nodes)
	c.artifacts.SetExecution(c.executions)
	c.artifacts.SetRecoveryResolutionDriver(NewWorkspaceRecoveryControl(c).Drive)
	values, cancel := context.WithCancel(t.Context())
	defer cancel()
	nodes.before = func(req ops.Request) error {
		if req.Op == ops.Apply {
			if _, err := c.tasks.SetAside(source.Execution.TaskID, task.StateCancelled); err != nil {
				return err
			}
			cancel()
		}
		return nil
	}
	land, err := c.artifacts.LandRecoveryResolutionOnce(values, *stuck.Resolution, id, source)
	if err != nil || land.State != artifact.LandCommitted {
		t.Fatalf("source cancel abandoned admitted resolver WAL: %+v %v", land, err)
	}
	c.owners, err = newChannelOwners("new-owner", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.maintaining = true
	if replayed, err := NewWorkspaceRecoveryControl(c).Replay(t.Context(), episode.ID); err != nil || !replayed {
		t.Fatalf("old committed decision required fresh owner/task authority: %v %v", replayed, err)
	}
	if nodes.applies != 1 {
		t.Fatalf("replay created another apply under revoked authority: %d", nodes.applies)
	}
	current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || current.Phase != "released" {
		t.Fatalf("old legal WAL result was not accepted: %+v %v", current, err)
	}
}

type recoveryCopyCleaner interface {
	CleanupRecoveryCopy(context.Context, string, ledger.Lease) (attempt.WorkspaceRecovery, error)
}

func releasedRecoveryCopy(t *testing.T) (*Coordinator, project.Project, project.Workspace, attempt.WorkspaceRecovery) {
	t.Helper()
	c, p, source, ws := sharedCopy(t)
	publishBoundRecoveryTurn(t, c, p, ws, "cleanup-source", map[string]string{"landed": "keep landed\n"})
	episode := captureBoundCopy(t, c, source)
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		if _, err := c.artifacts.LandRecoveryOnce(ctx, episode.ID, driver); err != nil {
			return err
		}
		var err error
		episode, err = c.artifacts.ReleaseRecovery(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c, p, ws, episode
}

func TestReleasedRecoveryCopyKeepsItsContainerClaimUntilExactRemovalIsRecorded(t *testing.T) {
	c, p, ws, episode := releasedRecoveryCopy(t)
	cleaner, ok := any(c.artifacts).(recoveryCopyCleaner)
	if !ok {
		t.Fatal("released recovery has no exact owned-container removal consumer")
	}
	other := project.Project{ID: "other", Home: project.Home{Node: ws.Node, Path: filepath.Dir(ws.Path)}}
	if err := c.projects.Declare(t.Context(), []project.Project{p, other}); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("another project claimed pending owned container: %v", err)
	}
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_copy_removed BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND json_extract(NEW.data,'$.copy_removed_at') IS NOT NULL BEGIN SELECT RAISE(ABORT,'copy removed refused'); END`)
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		if _, err := cleaner.CleanupRecoveryCopy(ctx, episode.ID, driver); err == nil {
			t.Fatal("refused deletion confirmation became success")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(ws.Path)); !os.IsNotExist(err) {
		t.Fatalf("owned container was not precisely removed: %v", err)
	}
	if err := c.projects.Declare(t.Context(), []project.Project{p, other}); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("physical delete before its Tx released container claim: %v", err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_copy_removed"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := cleaner.CleanupRecoveryCopy(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.projects.Declare(t.Context(), []project.Project{p, other}); err != nil {
		t.Fatalf("exact deletion acknowledgement did not end container claim: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "landed")); err != nil || string(raw) != "keep landed\n" {
		t.Fatalf("cleanup touched original target: %q %v", raw, err)
	}
}

func TestReleasedRecoveryCopyRefusesUnknownMarkerAndKeepsAdjacentContent(t *testing.T) {
	c, p, ws, episode := releasedRecoveryCopy(t)
	cleaner, ok := any(c.artifacts).(recoveryCopyCleaner)
	if !ok {
		t.Fatal("recovery has no exact marker-controlled cleanup consumer")
	}
	marker := filepath.Join(filepath.Dir(ws.Path), ".steve-workspace")
	if err := os.WriteFile(marker, []byte("another owner\n"), 0600); err != nil {
		t.Fatal(err)
	}
	adjacent := filepath.Join(filepath.Dir(filepath.Dir(ws.Path)), "adjacent-owned-fixture")
	if err := os.Mkdir(adjacent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adjacent, "preserve"), []byte("adjacent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewWorkspaceRecoveryControl(c).Drive(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := cleaner.CleanupRecoveryCopy(ctx, episode.ID, driver)
		if err == nil {
			t.Fatal("wrong marker authorized container deletion")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{marker, filepath.Join(adjacent, "preserve"), filepath.Join(p.Home.Path, "landed")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("marker refusal lost exact protected path %s: %v", path, err)
		}
	}
}
