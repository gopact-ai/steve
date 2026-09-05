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

func (r *Repo) checkLimits(ctx context.Context, workTree string, env, paths []string) error {
	// Use the snapshot's index and ignore rules, not the user's index.
	// Tracked files still count when ignored; deleted files do not.
	args := append([]string{"ls-files", "--cached", "--others", "--exclude-standard", "-z", "--"}, paths...)
	out, err := r.git(ctx, env, args...)
	if err != nil {
		return err
	}
	var files, bytes, largest int64
	for _, path := range strings.Split(out, "\x00") {
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
