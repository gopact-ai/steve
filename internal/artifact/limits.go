package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// Nodes expose command output even when their transport wraps the exit
// error. A reserved marker carries the typed error across that boundary.
const (
	snapshotTooLargeExit = 73
	snapshotTooLargeMark = "STEVE_SNAPSHOT_TOO_LARGE"
)

func snapshotTooLarge(out string, err error) (TooLarge, bool) {
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != snapshotTooLargeExit {
		return TooLarge{}, false
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != snapshotTooLargeMark {
			continue
		}
		switch fields[1] {
		case "files", "bytes", "file_bytes":
		default:
			continue
		}
		have, herr := strconv.ParseInt(fields[2], 10, 64)
		limit, lerr := strconv.ParseInt(fields[3], 10, 64)
		if herr == nil && lerr == nil && limit > 0 && have > limit {
			return TooLarge{Which: fields[1], Have: have, Limit: limit}, true
		}
	}
	return TooLarge{}, false
}

// checkLimits counts the same files as the local preflight. NUL-delimited
// paths survive spaces and newlines; du counts apparent bytes, including
// every hardlink, and never follows symlinks out of the workspace.
func (s Script) checkLimits(flatten bool) string {
	l := s.Limits.defaults()
	skipNested := ""
	if !flatten {
		skipNested = `
        parent=$path
        skip=
        while [ "${parent%/*}" != "$parent" ]; do
            parent=${parent%/*}
            if [ -e "$parent/.git" ] || [ -L "$parent/.git" ]; then skip=1; break; fi
        done
        [ -z "$skip" ] || continue`
	}
	filter := `for path do
        [ -f "$path" ] || [ -L "$path" ] || continue
        ` + skipNested + `
        printf './%s\0' "$path"
    done`
	return fmt.Sprintf(`git ls-files --cached --others --exclude-standard -z > "$GIT_INDEX_FILE.candidates" &&
xargs -0 -r sh -c %s sh < "$GIT_INDEX_FILE.candidates" > "$GIT_INDEX_FILE.files" &&
du -b -l -0 --files0-from="$GIT_INDEX_FILE.files" > "$GIT_INDEX_FILE.sizes" &&
awk -v RS='\0' '
    { size = $0; sub(/\t.*/, "", size); size += 0; files++; bytes += size; if (size > largest) largest = size }
    function refuse(which, have, limit, label) {
        printf "%s %%s %%.0f %%.0f\n", which, have, limit
        printf "快照%%s超出上限：%%.0f，上限 %%.0f；把大文件挪出工作区或加进 .gitignore\n", label, have, limit
        exit %d
    }
    END {
        if (files > %d) refuse("files", files, %d, "文件数")
        if (bytes > %d) refuse("bytes", bytes, %d, "总字节数")
        if (largest > %d) refuse("file_bytes", largest, %d, "最大单文件字节数")
    }' "$GIT_INDEX_FILE.sizes"`, quote(filter), snapshotTooLargeMark, snapshotTooLargeExit, l.MaxFiles, l.MaxFiles, l.MaxBytes, l.MaxBytes, l.MaxFileBytes, l.MaxFileBytes)
}
