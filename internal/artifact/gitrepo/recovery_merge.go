package gitrepo

import (
	"context"
	"strings"
)

// MergeRecovery uses base's tree as the unique common baseline even when the
// real sides share a newer ancestor. Synthetic parents stay in this shadow
// repository; only a result parented on the real sides is returned and pinned.
func (r *Repo) MergeRecovery(ctx context.Context, base, ours, theirs, message string) (sha, marked string, conflicts []string, err error) {
	baseTree, err := r.Git(ctx, nil, "rev-parse", base+"^{tree}")
	if err != nil {
		return "", "", nil, err
	}
	oursTree, err := r.Git(ctx, nil, "rev-parse", ours+"^{tree}")
	if err != nil {
		return "", "", nil, err
	}
	theirsTree, err := r.Git(ctx, nil, "rev-parse", theirs+"^{tree}")
	if err != nil {
		return "", "", nil, err
	}
	if theirsTree == baseTree || theirsTree == oursTree {
		return ours, "", nil, nil
	}
	left, err := r.Git(ctx, nil, "commit-tree", strings.TrimSpace(oursTree), "-p", base, "-m", "recovery current side")
	if err != nil {
		return "", "", nil, err
	}
	right, err := r.Git(ctx, nil, "commit-tree", strings.TrimSpace(theirsTree), "-p", base, "-m", "recovery incoming side")
	if err != nil {
		return "", "", nil, err
	}
	merged, conflicted, paths, err := r.MergeMarking(ctx, base, strings.TrimSpace(left), strings.TrimSpace(right), message)
	if err != nil {
		return "", "", nil, err
	}
	result := merged
	if conflicted != "" {
		result = conflicted
	}
	tree, err := r.Git(ctx, nil, "rev-parse", result+"^{tree}")
	if err != nil {
		return "", "", nil, err
	}
	commit, err := r.Git(ctx, nil, "commit-tree", strings.TrimSpace(tree), "-p", ours, "-p", theirs, "-m", message)
	if err != nil {
		return "", "", nil, err
	}
	result = strings.TrimSpace(commit)
	if err := r.pinNew(ctx, result); err != nil {
		return "", "", nil, err
	}
	if len(paths) > 0 {
		return "", result, paths, nil
	}
	return result, "", nil, nil
}
