package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	var candidates []string
	if node == "" {
		entries, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		cutoff := s.now().Add(-SweepAge)
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "wt-") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			candidates = append(candidates, filepath.Join(base, e.Name()))
		}
	} else {
		out, err := s.nodes.Exec(ctx, node, "", fmt.Sprintf("test -d %s && find %s -mindepth 1 -maxdepth 1 -type d -name 'wt-*' -mmin +%d -print || true",
			quote(base), quote(base), int(SweepAge.Minutes())))
		if err != nil {
			return nil, fmt.Errorf("list worktrees on %s: %w", node, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && strings.HasPrefix(filepath.Base(line), "wt-") && strings.HasPrefix(line, base) {
				candidates = append(candidates, line)
			}
		}
	}
	var removed []string
	for _, path := range candidates {
		if keep != nil && keep(path) {
			continue
		}
		if node == "" {
			if err := os.RemoveAll(path); err != nil {
				return removed, err
			}
		} else if _, err := s.nodes.Exec(ctx, node, "", "rm -rf "+quote(path)); err != nil {
			return removed, fmt.Errorf("remove %s on %s: %w", path, node, err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}
