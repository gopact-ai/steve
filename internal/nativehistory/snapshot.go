package nativehistory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/fsx"
)

type ImportRequest struct {
	CommandID string `json:"command_id"`
	Source    Source `json:"source"`
	NativeID  string `json:"native_id"`
	Revision  string `json:"revision"`
	Workdir   string `json:"workdir,omitempty"`
}

// Snapshot freezes a selected history under a caller-owned idempotency key.
// Repeating a completed import reads its receipt even if the source is gone.
func Snapshot(ctx context.Context, store string, request ImportRequest) (Reference, error) {
	if request.CommandID == "" || len(request.CommandID) > 512 || !validSourceNativeID(request.Source.Harness, request.NativeID) || request.Revision == "" {
		return Reference{}, errors.New("native import requires a command, session id and source revision")
	}
	release, err := LockStorage(ctx, store)
	if err != nil {
		return Reference{}, err
	}
	defer release()
	sum := sha256.Sum256([]byte(request.CommandID))
	id := "import_" + hex.EncodeToString(sum[:])
	dest := filepath.Join(store, id)
	if previous, err := readReference(dest); err == nil {
		if !matchesRequest(previous, request) {
			return Reference{}, errors.New("native import command already used with different input")
		}
		if err := checkWorkdir(previous.SourceWorkdir, request.Workdir); err != nil {
			return Reference{}, err
		}
		return previous, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Reference{}, err
	}
	entries, err := List(ctx, request.Source)
	if err != nil {
		return Reference{}, err
	}
	var selected *Entry
	for i := range entries {
		if entries[i].NativeID != request.NativeID {
			continue
		}
		if selected != nil {
			return Reference{}, errors.New("native session id is ambiguous in the selected history home")
		}
		selected = &entries[i]
	}
	if selected == nil || selected.Revision != request.Revision {
		return Reference{}, ErrChanged
	}
	if err := checkWorkdir(selected.Workdir, request.Workdir); err != nil {
		return Reference{}, err
	}
	if err := CheckStorage(ctx, store, 1, MaxSnapshotBytes); err != nil {
		return Reference{}, err
	}
	root, err := sourceRoot(request.Source)
	if err != nil {
		return Reference{}, err
	}
	defer root.Close()
	files, err := inventory(ctx, root, *selected, true)
	if err != nil {
		return Reference{}, err
	}
	if inventoryRevision(*selected, files) != request.Revision {
		return Reference{}, ErrChanged
	}
	if err := os.MkdirAll(store, 0700); err != nil {
		return Reference{}, err
	}
	stage, err := os.MkdirTemp(store, ".import-")
	if err != nil {
		return Reference{}, err
	}
	defer os.RemoveAll(stage)
	digest, err := copyHistory(ctx, root, filepath.Join(stage, "history"), files)
	if err != nil {
		return Reference{}, err
	}
	current, err := inventory(ctx, root, *selected)
	if err != nil {
		return Reference{}, err
	}
	if inventoryRevision(*selected, current) != selected.Revision {
		return Reference{}, ErrChanged
	}
	ref := Reference{ID: id, Harness: selected.Harness, NativeID: selected.NativeID, SourceHome: selected.SourceHome,
		SourceWorkdir: selected.Workdir, Revision: selected.Revision, Digest: digest, ImportedAt: time.Now().UTC()}
	raw, err := json.Marshal(ref)
	if err != nil {
		return Reference{}, err
	}
	if err := writeSynced(filepath.Join(stage, "receipt.json"), raw); err != nil {
		return Reference{}, err
	}
	if err := syncDirectory(stage); err != nil {
		return Reference{}, err
	}
	if err := CheckStorage(ctx, store, 0, 0); err != nil {
		return Reference{}, err
	}
	if err := os.Rename(stage, dest); err != nil {
		previous, readErr := readReference(dest)
		if readErr == nil && matchesRequest(previous, request) {
			return previous, nil
		}
		return Reference{}, err
	}
	if err := syncDirectory(store); err != nil {
		return Reference{}, err
	}
	return ref, nil
}

func checkWorkdir(source, destination string) error {
	if destination != "" && (!filepath.IsAbs(destination) || filepath.Clean(source) != filepath.Clean(destination)) {
		return errors.New("native history requires its original workspace")
	}
	return nil
}

func matchesRequest(ref Reference, req ImportRequest) bool {
	return ref.Harness == req.Source.Harness && ref.SourceHome == req.Source.Home && ref.NativeID == req.NativeID && ref.Revision == req.Revision
}

func readReference(dir string) (Reference, error) {
	var ref Reference
	raw, err := os.ReadFile(filepath.Join(dir, "receipt.json"))
	if err != nil {
		return ref, err
	}
	err = json.Unmarshal(raw, &ref)
	return ref, err
}

func copyHistory(ctx context.Context, root *os.Root, dest string, files []sourceFile) (string, error) {
	h := sha256.New()
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := copyHistoryFile(ctx, root, dest, f, h); err != nil {
			return "", err
		}
	}
	// Synchronize each directory before publishing the containing import.
	err := filepath.WalkDir(dest, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return syncDirectory(path)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

func copyHistoryFile(ctx context.Context, root *os.Root, dest string, selected sourceFile, h hash.Hash) error {
	// Reject links in every component. OpenRoot additionally confines any
	// replacement between this check and open to the selected source home.
	parts := strings.Split(filepath.ToSlash(selected.path), "/")
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrChanged
		}
	}
	source, err := root.Open(selected.path)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !sameSourceFile(selected.info, info) {
		return ErrChanged
	}
	path := filepath.Join(dest, selected.path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	_ = json.NewEncoder(h).Encode([]any{selected.path, info.Size()})
	reader := &contextReader{ctx: ctx, reader: io.LimitReader(source, info.Size()+1)}
	writers := []io.Writer{out, h}
	var attachmentHash hash.Hash
	if strings.HasPrefix(filepath.ToSlash(selected.path), "attachments/v1/objects/") {
		attachmentHash = sha256.New()
		writers = append(writers, attachmentHash)
	}
	n, err := io.Copy(io.MultiWriter(writers...), reader)
	if err != nil {
		return err
	}
	if n != info.Size() {
		return ErrChanged
	}
	if attachmentHash != nil && hex.EncodeToString(attachmentHash.Sum(nil)) != filepath.Base(selected.path) {
		return errors.New("DSH attachment content does not match its SHA-256 reference")
	}
	current, err := source.Stat()
	if err != nil {
		return err
	}
	if !sameSourceFile(info, current) {
		return ErrChanged
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

func sameSourceFile(before, after os.FileInfo) bool {
	return after.Mode().IsRegular() && os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime() == after.ModTime()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func writeSynced(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	return f.Sync()
}

func syncDirectory(path string) error {
	if err := fsx.SyncDir(path); err != nil {
		return fmt.Errorf("sync native import: %w", err)
	}
	return nil
}
