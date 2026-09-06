package artifact

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
)

// SweepAge is how old a worktree must be before a sweep may take it: a
// directory younger than this may still be materialising for an attempt
// that has not been recorded yet.
const SweepAge = 10 * time.Minute

// SweepWorktrees removes the isolated worktrees under root that nothing
// live owns: a child whose machine dropped mid-run, a hub that restarted
// with attempts in flight, leave their directories behind, and nothing
// else ever comes back for them. keep says whether a worktree path is
// still an attempt's; only `worktrees/wt-*` directories older than
// SweepAge are considered. node "" is the hub, where root is the
// store's own directory.
func (s *Store) SweepWorktrees(ctx context.Context, node, root string, keep func(path string) bool) ([]string, error) {
	if node == "" {
		root = s.Dir
	}
	if root == "" {
		return nil, nil
	}
	base := filepath.Join(root, "worktrees")
	result, err := s.operation(ctx, node, ops.Request{Op: ops.ListWorktrees, WorkTree: base, Before: s.now().Add(-SweepAge)})
	if err != nil {
		return nil, fmt.Errorf("list worktrees on %s: %w", placeName(node), err)
	}
	var removed []string
	for _, path := range result.Paths {
		if keep != nil && keep(path) {
			continue
		}
		if _, err := s.operation(ctx, node, ops.Request{Op: ops.Remove, Path: path}); err != nil {
			return removed, fmt.Errorf("remove %s on %s: %w", path, placeName(node), err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}
