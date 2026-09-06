package artifact

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// FileContent reads the exact immutable blob at a snapshot. It never follows
// a worktree path or symlink, and refuses truncation for captured references.
func (s *Store) FileContent(ctx context.Context, projectID, commit, name string, maxBytes int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Review.defaults().Timeout)
	defer cancel()
	if name == "" || path.IsAbs(name) || strings.Contains(name, "\\") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || maxBytes <= 0 {
		return nil, errors.New("invalid snapshot file reference")
	}
	r, err := s.reviewRepo(ctx, projectID, "", commit)
	if err != nil {
		return nil, err
	}
	spec := commit + ":" + name
	kind, err := r.git(ctx, nil, "cat-file", "-t", spec)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(kind) != "blob" {
		return nil, errors.New("snapshot reference is not a file")
	}
	sizeText, err := r.git(ctx, nil, "cat-file", "-s", spec)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64)
	if err != nil || size < 0 || size > maxBytes {
		return nil, fmt.Errorf("snapshot file exceeds capture limit of %d bytes", maxBytes)
	}
	data, err := r.gitBytes(ctx, int(maxBytes)+1, "cat-file", "-p", spec)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, errors.New("snapshot file was not read completely")
	}
	return data, nil
}
