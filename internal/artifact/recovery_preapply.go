package artifact

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func retryableRecoveryPreapply(land Landing) bool {
	return land.State == LandApplyConflicted && land.Recovery != nil && land.Recovery.Resolution == nil &&
		land.Unapplied && land.Round == 0 && land.Conflict == "" && land.Committed == nil
}

// Only an unwritten, unmarked root may merge again. Its exact Pending and
// transition are one fenced transaction; fresh apply still rechecks every source.
func (s *Store) retryRecoveryPreapply(ctx context.Context, p project.Project, land Landing) (Landing, error) {
	retry := land
	retry.State, retry.Lease, retry.Now, retry.Merged, retry.Paths = LandProposed, nil, "", "", nil
	retry.Unapplied, retry.Borrowed, retry.Error, retry.EndedAt = false, false, "", time.Time{}
	_, err := s.ledger.Transition(ctx, land.ID, LandApplyConflicted, LandProposed, land.By, landingFence(ctx, nil), nil,
		func(tx *ledger.Tx, op *ledger.Operation) error {
			var stored Landing
			if json.Unmarshal(op.Data, &stored) != nil {
				return ErrNotBlocked
			}
			stored.State = op.State
			if !retryableRecoveryPreapply(stored) || !reflect.DeepEqual(stored, land) {
				return ErrNotBlocked
			}
			r, err := checkRecoveryLandingPermitTx(ctx, tx, p, &stored, nil, true)
			if err != nil {
				return err
			}
			if err := clearRecoveryPreapplyPendingTx(tx, r, stored); err != nil {
				return err
			}
			return tx.SetData(op, retry)
		})
	if err != nil {
		return land, err
	}
	return retry, nil
}

func clearRecoveryPreapplyPendingTx(tx *ledger.Tx, r attempt.WorkspaceRecovery, land Landing) error {
	var winner []byte
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, recoveryWinnerKind, attempt.RecoveryLandingID(r)).Scan(&winner)
	if err == nil {
		return ErrNotBlocked
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	id := r.Project + "/" + r.Head.Artifact
	var raw []byte
	err = tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, pendingKind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var item Pending
	expected := attempt.RecoveryResolutionRef{Episode: r.ID, HeadVersion: r.FrozenHeadVersion, Root: attempt.RecoveryLandingID(r), Conflict: land.ID, Canonical: land.Now}
	if json.Unmarshal(raw, &item) != nil || item.Project != r.Project || item.Artifact != r.Head.Artifact || item.Source != nil || !sameRecoveryLink(item.Recovery, land.Recovery) || item.Resolution == nil || *item.Resolution != expected || item.Blocked == nil || item.Blocked.Landing != land.ID || item.Blocked.State != land.State || item.Blocked.Canonical != land.Now || item.Blocked.Marked != "" || !reflect.DeepEqual(item.Blocked.Paths, land.Paths) {
		return ErrNotBlocked
	}
	_, err = tx.Exec(`DELETE FROM bindings WHERE kind=? AND id=?`, pendingKind, id)
	return err
}

func (s *Store) recoveryPreapply(ctx context.Context, p project.Project, land *Landing) error {
	// PathState compares the physical entity as well as both trees. A path
	// absent from a snapshot can still hold an ignored or inaccessible entity.
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return err
	}
	if err := s.stageMerged(ctx, p, land, repo); err != nil {
		return err
	}
	_, _, conflicted, err := s.inspectPaths(ctx, p, *land, land.Paths)
	if err != nil {
		return err
	}
	if len(conflicted) == 0 {
		return nil
	}
	reason := "affected original paths contain uncaptured content or changed type"
	land.Unapplied = true
	if err := s.fail(ctx, land, LandLocked, LandApplyConflicted, reason, conflicted); err != nil {
		return err
	}
	return Conflict{State: LandApplyConflicted, Paths: conflicted, Reason: reason}
}

func (s *Store) mergeRecoveryOnNode(ctx context.Context, p project.Project, base, ours, theirs string) (string, string, []string, error) {
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return "", "", nil, err
	}
	result, err := s.nodes.Artifact(ctx, p.Home.Node, ops.Request{Op: ops.MergeRecovery, Repo: nodeBare(state, p.ID), Base: base, Ours: ours, Theirs: theirs, Message: "land recovery into " + p.ID})
	if err != nil {
		var conflict gitrepo.MergeConflict
		if errors.As(err, &conflict) {
			return "", conflict.Marked, conflict.Paths, nil
		}
		return "", "", nil, err
	}
	return result.Commit, "", nil, nil
}
