package turn

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/project"
)

func TestRecoveryReservationBeforeNativePreparationCanBeReleasedWithoutChangingFiles(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	r, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "never-prepared"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FailWith(t.Context(), r.ID, "fixture", "admission refused", nil); err != nil {
		t.Fatal(err)
	}
	_, err = c.workspaceFor(t.Context(), Request{
		Source: Source{ConversationID: "console:retry"},
		Actor:  Actor{ID: "owner"},
	}, agent.Agent{ID: "worker", Node: "node", Harness: "mock"}, project.Binding{ProjectID: p.ID})
	if err != nil {
		t.Fatalf("an unprepared failed input permanently held the copy: %v", err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Producer != nil || episode.Head.Version != 1 {
		t.Fatalf("releasing unused reservation changed accepted work: %+v %v", episode, err)
	}
}

func TestRecoveryPreparationAndLostOpenKeepTheirMonotonicWriteObligation(t *testing.T) {
	for _, arm := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "open may have started"}[arm], func(t *testing.T) {
			c, p, source, ws := sharedCopy(t)
			r, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "prepared-input"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.attempts.Advance(t.Context(), r.ID, attempt.Prepared, "fixture", nil); err != nil {
				t.Fatal(err)
			}
			if arm {
				if err := c.attempts.ArmSession(t.Context(), r.ID, "fixture"); err != nil {
					t.Fatal(err)
				}
				if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.attempts.FailWith(t.Context(), r.ID, "fixture", "preparation failed after authorization", nil); err != nil {
				t.Fatal(err)
			}
			_, err = c.workspaceFor(t.Context(), Request{
				Source: Source{ConversationID: "console:retry"},
				Actor:  Actor{ID: "owner"},
			}, agent.Agent{ID: "worker", Node: "node", Harness: "mock"}, project.Binding{ProjectID: p.ID})
			if !errors.Is(err, attempt.ErrWorkspaceRecovery) {
				t.Fatalf("possible native write without output lost its fence: %v", err)
			}
			episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
			if err != nil || episode.Producer == nil || episode.Producer.NativeMayWrite == nil || !*episode.Producer.NativeMayWrite {
				t.Fatalf("preparation was not durably marked before external work: %+v %v", episode, err)
			}
		})
	}
}

func TestRecoveryWriteGateCommitsBeforePreparationAndRejectsWithoutExternalWork(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "reject"}[rejected], func(t *testing.T) {
			c, p, source, ws := sharedCopy(t)
			r, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "preparing-input"))
			if err != nil {
				t.Fatal(err)
			}
			if rejected {
				forceStopTrigger(t, c, `CREATE TRIGGER refuse_write_gate BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND json_extract(NEW.data,'$.producer.native_may_write')=1 BEGIN SELECT RAISE(ABORT,'write gate refused'); END`)
			}
			called := false
			turn := &chatTurn{c: c, workspace: ws, clock: newTurnClock(), req: Request{OnTurnReady: func(_, _ string) {
				called = true
				episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
				if err != nil || episode.Producer == nil || episode.Producer.NativeMayWrite == nil || !*episode.Producer.NativeMayWrite {
					t.Fatal("external preparation began before its durable write obligation")
				}
			}}}
			_, err = turn.prepare(t.Context(), &lifecycle.Execution{Record: r})
			if rejected && (err == nil || called) {
				t.Fatalf("rejected write gate permitted external preparation: called=%v err=%v", called, err)
			}
			if !rejected && (err != nil || !called) {
				t.Fatalf("committed preparation did not run: %v", err)
			}
			episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
			if err != nil || episode.Producer.NativeMayWrite == nil || *episode.Producer.NativeMayWrite == rejected {
				t.Fatalf("write gate differs from the committed decision: %+v %v", episode, err)
			}
		})
	}
}

func TestRecoveryArmWithoutPreparedStillRetainsItsWriteObligation(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	r, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "armed-only"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.ArmSession(t.Context(), r.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), r.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FailWith(t.Context(), r.ID, "fixture", "lost open", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.PublishRecoveryHead(t.Context(), source.Abandoned.WorkspaceRecoveryID, c.artifacts.RecoveryOutputTx); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("armed input without a candidate released its copy: %v", err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Producer == nil || !*episode.Producer.NativeMayWrite {
		t.Fatalf("settled erased possible write authorization: %+v %v", episode, err)
	}
}

func TestUnusedRecoveryReleaseRequiresExplicitSettlementAndPreservesFiles(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	r, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "unused-input"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "unpublished"), []byte("preserve without inspection"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.Advance(t.Context(), r.ID, attempt.Failed, "fixture", func(r *attempt.Record) { r.SessionSettled = nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.PublishRecoveryHead(t.Context(), source.Abandoned.WorkspaceRecoveryID, c.artifacts.RecoveryOutputTx); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("missing settlement proof released a reservation: %v", err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE operations SET data=json_set(data,'$.session_settled',json('true')) WHERE id=?`, r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.PublishRecoveryHead(t.Context(), source.Abandoned.WorkspaceRecoveryID, c.artifacts.RecoveryOutputTx); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(ws.Path, "unpublished")); err != nil || string(raw) != "preserve without inspection" {
		t.Fatalf("unused release changed files: %q %v", raw, err)
	}
}
