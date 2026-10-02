package turn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
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
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), original.Abandoned.WorkspaceRecoveryID, func(ctx context.Context, driver ledger.Lease) error {
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
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
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
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
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
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		_, err := capture.CaptureRecoveryResidual(ctx, episode.ID, driver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if count() != before+1 {
		t.Fatal("retry did not accept precisely one captured artifact")
	}
}
