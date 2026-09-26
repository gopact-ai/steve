package gitrepo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/budget"
	"github.com/gopact-ai/steve/internal/i18n"
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
		l.MaxFiles = budget.SnapshotFiles
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = budget.SnapshotBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = budget.SnapshotFileBytes
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

// Error is the English diagnostic: the budget may be exceeded on another
// node, where nobody's language is known. Say tells a reader.
func (e TooLarge) Error() string { return e.Say(i18n.New(i18n.LocaleEN)) }

// Say explains the exceeded budget, its sizes and the remedy in text's
// language.
func (e TooLarge) Say(text i18n.Catalog) string {
	which := map[string]i18n.Key{"files": i18n.SnapshotLimitFiles, "bytes": i18n.SnapshotLimitBytes, "file_bytes": i18n.SnapshotLimitFileBytes}[e.Which]
	return text.T(i18n.SnapshotTooLarge, text.T(which), e.Have, e.Limit)
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
		if _, err := r.Git(ctx, env, append([]string{"update-index", "--force-remove", "--"}, links...)...); err != nil {
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
	out, err := r.Git(ctx, env, args...)
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
