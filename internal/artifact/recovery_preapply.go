package artifact

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/artifact/gitrepo"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/project"
)

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
