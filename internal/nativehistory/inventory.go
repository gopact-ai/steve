package nativehistory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type sourceFile struct {
	path string
	info os.FileInfo
}

// inventory includes only the chosen transcript and its own adjunct history.
// It rejects links and special files, including links inside adjunct trees.
func inventory(ctx context.Context, root *os.Root, entry Entry, includeAttachments ...bool) ([]sourceFile, error) {
	var files []sourceFile
	var size int64
	add := func(path string, info os.FileInfo) error {
		if !info.Mode().IsRegular() {
			return errors.New("native history contains a link or special file")
		}
		size += info.Size()
		if size > MaxSnapshotBytes || len(files) >= 10000 {
			return errors.New("selected native history exceeds the snapshot limit")
		}
		files = append(files, sourceFile{path, info})
		return nil
	}
	walk := func(path string) error {
		return fs.WalkDir(root.FS(), path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			return add(path, info)
		})
	}
	if err := walk(entry.path); err != nil {
		return nil, err
	}
	if entry.Harness == "claude-code" {
		adjunct := strings.TrimSuffix(entry.path, ".jsonl")
		if _, err := root.Lstat(adjunct); err == nil {
			if err := walk(adjunct); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if entry.Harness == "grok" {
		cwd := filepath.Join(filepath.Dir(entry.path), ".cwd")
		if info, err := root.Lstat(cwd); err == nil {
			if err := add(cwd, info); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if entry.Harness == "dsh" && len(includeAttachments) > 0 && includeAttachments[0] {
		attachments, err := dshAttachments(ctx, root, files)
		if err != nil {
			return nil, err
		}
		for _, path := range attachments {
			info, err := root.Lstat(path)
			if err != nil {
				return nil, err
			}
			if err := add(path, info); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(files, func(a, b sourceFile) int { return strings.Compare(a.path, b.path) })
	return files, nil
}

func inventoryRevision(entry Entry, files []sourceFile) string {
	h := sha256.New()
	encoder := json.NewEncoder(h)
	_ = encoder.Encode([]string{entry.Harness, entry.NativeID, entry.SourceHome, entry.Workdir})
	for _, f := range files {
		// Discovery fingerprints transcript metadata. DSH's content-addressed
		// attachments are extracted only for the selected snapshot and their
		// bytes are verified against the reference digest during copying.
		if entry.Harness == "dsh" && strings.HasPrefix(filepath.ToSlash(f.path), "attachments/") {
			continue
		}
		_ = encoder.Encode([]any{f.path, f.info.Size(), f.info.ModTime().UTC()})
	}
	return hex.EncodeToString(h.Sum(nil))
}
