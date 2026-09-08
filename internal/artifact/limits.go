package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Default snapshot budgets bound staging before git starts reading blobs.
const (
	MaxSnapshotFiles     = 20_000
	MaxSnapshotBytes     = 2 * 1024 * 1024 * 1024
	MaxSnapshotFileBytes = 200 * 1024 * 1024
)

// Limits can be injected per repository or store. Zero fields use the
// defaults, so existing callers also get protection.
type Limits struct {
	MaxFiles     int64
	MaxBytes     int64
	MaxFileBytes int64
}

func (l Limits) defaults() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = MaxSnapshotFiles
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = MaxSnapshotBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = MaxSnapshotFileBytes
	}
	return l
}

// TooLarge identifies the exceeded snapshot budget. Which is files,
// bytes (total), or file_bytes (the largest individual file).
type TooLarge struct {
	Which string
	Have  int64
	Limit int64
}

func (e TooLarge) Error() string {
	which := map[string]string{"files": "文件数", "bytes": "总字节数", "file_bytes": "最大单文件字节数"}[e.Which]
	return fmt.Sprintf("快照%s超出上限：%d，上限 %d；把大文件挪出工作区或加进 .gitignore", which, e.Have, e.Limit)
}

func (l Limits) check(files, bytes, largest int64) error {
	for _, v := range []TooLarge{{"files", files, l.MaxFiles}, {"bytes", bytes, l.MaxBytes}, {"file_bytes", largest, l.MaxFileBytes}} {
		if v.Have > v.Limit {
			return v
		}
	}
	return nil
}

// prepareSnapshotIndex removes inherited gitlinks and checks the candidate
// files before staging. One tagged index listing serves both checks on the
// common path; only a gitlink needs removal and another listing so its newly
// exposed files are counted too.
func (r *Repo) prepareSnapshotIndex(ctx context.Context, workTree string, env, paths []string) error {
	candidates, links, err := r.snapshotCandidates(ctx, env, paths)
	if err != nil {
		return err
	}
	if len(links) > 0 {
		if _, err := r.git(ctx, env, append([]string{"update-index", "--force-remove", "--"}, links...)...); err != nil {
			return fmt.Errorf("drop gitlinks: %w", err)
		}
		candidates, _, err = r.snapshotCandidates(ctx, env, paths)
		if err != nil {
			return err
		}
	}
	var files, bytes, largest int64
	for _, path := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "" {
			continue
		}
		info, err := os.Lstat(filepath.Join(workTree, path))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		files++
		bytes += info.Size()
		largest = max(largest, info.Size())
	}
	return r.Limits.defaults().check(files, bytes, largest)
}

// Use the snapshot's index and ignore rules, not the user's index.
// Tracked files still count when ignored; deleted files do not. The -t tag
// distinguishes an untracked filename that resembles staged-entry metadata.
func (r *Repo) snapshotCandidates(ctx context.Context, env, paths []string) (files, links []string, err error) {
	args := append([]string{"ls-files", "-t", "--stage", "--cached", "--others", "--exclude-standard", "-z", "--"}, paths...)
	out, err := r.git(ctx, env, args...)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		if path, ok := strings.CutPrefix(entry, "? "); ok {
			files = append(files, path)
			continue
		}
		metadata, path, ok := strings.Cut(entry, "\t")
		if !ok || len(metadata) < 2 {
			return nil, nil, fmt.Errorf("invalid snapshot index entry")
		}
		if strings.HasPrefix(metadata[2:], "160000 ") {
			links = append(links, path)
			continue
		}
		files = append(files, path)
	}
	return files, links, nil
}
