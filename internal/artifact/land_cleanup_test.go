package artifact

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestParentLeaseReleaseBeforeApplyClosesLandingAndAllowsExactResultRetry(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease-released", true: "caller-cancelled"}[cancelled], func(t *testing.T) {
			canonical := t.TempDir()
			write(t, canonical, "file", "before")
			local := &localNode{root: t.TempDir(), state: t.TempDir()}
			s, p := newStore(t, local, project.Home{Node: "node", Path: canonical})
			tasks, err := task.OpenLedger(s.ledger, "")
			if err != nil {
				t.Fatal(err)
			}
			child, err := tasks.Create(task.Task{Channel: "c"})
			if err != nil {
				t.Fatal(err)
			}
			token, err := tasks.ExecutionToken(child.ID)
			if err != nil {
				t.Fatal(err)
			}
			source := Source{Execution: &token, AttemptID: "child-attempt"}
			ws, err := s.Materialize(t.Context(), project.Request{Project: p.ID, Isolated: true, Owner: source.AttemptID})
			if err != nil {
				t.Fatal(err)
			}
			write(t, ws.Path, "file", "accepted")
			result, _, err := s.Publish(t.Context(), ws, ws.Base, source.AttemptID, "result")
			if err != nil {
				t.Fatal(err)
			}
			lease, err := s.ledger.Acquire(t.Context(), "canonical:"+p.ID, "parent", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fired := false
			s.nodes = &stoppingNode{localNode: local, before: func(op ops.Kind) {
				if fired || op != ops.Snapshot {
					return
				}
				fired = true
				if err := s.ledger.Release(t.Context(), lease); err != nil {
					t.Fatal(err)
				}
				if cancelled {
					cancel()
				}
			}}
			land, err := s.LandUnder(ctx, p, result.ID, "child", lease, source)
			if !fired || err == nil || read(t, canonical, "file") != "before" {
				t.Fatalf("barrier failed: %+v %v", land, err)
			}
			all, err := s.Landings(t.Context(), p.ID)
			if err != nil || len(all) != 1 || all[0].State != LandMergeConflicted || all[0].Lease != nil || all[0].Round != 0 || all[0].EndedAt.IsZero() {
				t.Fatalf("unfinished preapply record: %+v %v", all, err)
			}
			check := func() error {
				return s.ledger.Update(t.Context(), func(tx *ledger.Tx) error { return CheckTaskLandingsTx(tx, map[string]bool{child.ID: true}) })
			}
			if !errors.Is(check(), task.ErrCompleteDelivery) {
				t.Fatal("unlanded result allowed completion")
			}
			if _, err := s.Land(t.Context(), p, result.ID, "retry", source); err != nil {
				t.Fatal(err)
			}
			if read(t, canonical, "file") != "accepted" {
				t.Fatal("exact result was not delivered")
			}
			if err := check(); err != nil {
				t.Fatalf("accepted result still blocked by abandoned landing: %v", err)
			}
		})
	}
}

func TestPreapplyCleanupLeavesApplyingWALForRecovery(t *testing.T) {
	canonical := t.TempDir()
	s, p := newStore(t, &localNode{}, project.Home{Path: canonical})
	land, _ := crashMidApply(t, s, p, canonical)
	s.closeUnappliedLanding(t.Context(), &land, context.Canceled)
	op, _, err := s.ledger.Operation(t.Context(), land.ID)
	if err != nil || op.State != LandApplying {
		t.Fatalf("applying WAL was discarded: %+v %v", op, err)
	}
	if _, err := s.RecoverLandings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if read(t, canonical, "b") != "b1" {
		t.Fatal("remaining WAL paths did not recover")
	}
}
