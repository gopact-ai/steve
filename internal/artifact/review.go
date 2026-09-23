package artifact

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
)

// Tree lists a snapshot directory of a project the hub keeps objects for.
func (s *Store) Tree(ctx context.Context, projectID, commit, dir string) ([]gitrepo.Entry, bool, error) {
	repo, err := s.reviewRepo(ctx, projectID, "", commit)
	if err != nil {
		return nil, false, err
	}
	return repo.Tree(ctx, commit, dir)
}

// File reads one file of such a snapshot.
func (s *Store) File(ctx context.Context, projectID, commit, path string) (string, int64, bool, bool, error) {
	repo, err := s.reviewRepo(ctx, projectID, "", commit)
	if err != nil {
		return "", 0, false, false, err
	}
	return repo.File(ctx, commit, path)
}

// Changes is the index of an attempt's change, by project: what the
// snapshot pair differs in. A sealed project whose objects stay on its
// own machine has nothing the hub may show.
func (s *Store) Changes(ctx context.Context, projectID, from, to string) ([]gitrepo.Change, bool, error) {
	repo, err := s.reviewRepo(ctx, projectID, from, to)
	if err != nil {
		return nil, false, err
	}
	return repo.Changes(ctx, from, to)
}

// FileDiff is one file of that change. The path must be in the index:
// nothing is diffed that the index did not name.
func (s *Store) FileDiff(ctx context.Context, projectID, from, to, path string) (string, bool, error) {
	repo, err := s.reviewRepo(ctx, projectID, from, to)
	if err != nil {
		return "", false, err
	}
	changes, _, err := repo.Changes(ctx, from, to)
	if err != nil {
		return "", false, err
	}
	known := false
	for _, c := range changes {
		if c.Path == path {
			known = true
			if c.Binary {
				return "", false, fmt.Errorf("%s is binary; no text diff", path)
			}
			break
		}
	}
	if !known {
		return "", false, fmt.Errorf("%s is not among the changed files", path)
	}
	return repo.FileDiff(ctx, from, to, path)
}

func (s *Store) reviewRepo(ctx context.Context, projectID, from, to string) (*gitrepo.Repo, error) {
	limits, review := s.policy()
	return s.reviewRepoWithPolicy(ctx, projectID, from, to, limits, review)
}

func (s *Store) reviewRepoWithPolicy(ctx context.Context, projectID, from, to string, limits gitrepo.Limits, review gitrepo.ReviewLimits) (*gitrepo.Repo, error) {
	p, ok, err := s.projects.GetHistorical(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no project %q", projectID)
	}
	if metadataOnly(p) {
		return nil, fmt.Errorf("project %s is sealed and its data stays on %s; no diff here", p.ID, p.Home.Node)
	}
	for _, sha := range []string{from, to} {
		if sha != "" && !gitrepo.ValidSHA(sha) {
			return nil, fmt.Errorf("bad snapshot id %q", sha)
		}
	}
	if to == "" {
		return nil, errors.New("no after-snapshot: nothing changed, or the change was not captured")
	}
	return s.repoWithPolicy(ctx, projectID, limits, review)
}
