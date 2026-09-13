package nativehistory

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// DSH image references live in session events but their immutable objects live
// outside the session directory. Select only referenced objects from that home.
func dshAttachments(ctx context.Context, root *os.Root, files []sourceFile) ([]string, error) {
	objects := map[string]bool{}
	for _, file := range files {
		name := filepath.Base(file.path)
		if !isDshTranscript(name) {
			continue
		}
		selected, err := selectedDshGeneration(root, file.path)
		if err != nil {
			return nil, err
		}
		if !selected {
			continue
		}
		if err := scanDshAttachments(ctx, root, file.path, objects); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(objects))
	for digest := range objects {
		out = append(out, filepath.Join("attachments", "v1", "objects", digest[:2], digest))
	}
	slices.Sort(out)
	return out, nil
}

func scanDshAttachments(ctx context.Context, root *os.Root, path string, objects map[string]bool) error {
	file, err := root.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(path, ".zstd") {
		decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderMaxWindow(32<<20))
		if err != nil {
			return err
		}
		defer decoder.Close()
		reader = decoder
	}
	bounded := &io.LimitedReader{R: reader, N: MaxSnapshotBytes + 1}
	scan := bufio.NewScanner(bounded)
	scan.Buffer(make([]byte, 4096), 32<<20)
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var value any
		if err := json.Unmarshal(scan.Bytes(), &value); err != nil {
			return err
		}
		if err := collectDshAttachments(value, objects, 0); err != nil {
			return err
		}
	}
	if bounded.N == 0 {
		return errors.New("DSH decoded history exceeds the snapshot limit")
	}
	return scan.Err()
}

func collectDshAttachments(value any, objects map[string]bool, depth int) error {
	if depth > 128 || len(objects) > 10000 {
		return errors.New("DSH attachment references exceed the snapshot limit")
	}
	switch v := value.(type) {
	case map[string]any:
		if raw, exists := v["attachmentId"]; exists {
			id, ok := raw.(string)
			digest, prefixed := strings.CutPrefix(id, "sha256:")
			_, err := hex.DecodeString(digest)
			if !ok || !prefixed || len(digest) != 64 || strings.ToLower(digest) != digest || err != nil {
				return errors.New("invalid DSH attachment reference")
			}
			objects[digest] = true
		}
		for _, item := range v {
			if err := collectDshAttachments(item, objects, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := collectDshAttachments(item, objects, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func isDshTranscript(name string) bool {
	_, ok := dshGeneration(name)
	return ok
}

func dshGeneration(name string) (int, bool) {
	name = strings.TrimSuffix(name, ".zstd")
	if name == "session.jsonl" {
		return 0, true
	}
	version, ok := strings.CutPrefix(name, "session.v")
	if !ok || !strings.HasSuffix(version, ".jsonl") {
		return 0, false
	}
	version = strings.TrimSuffix(version, ".jsonl")
	n, err := strconv.Atoi(version)
	return n, err == nil && n > 0 && strconv.Itoa(n) == version
}

// DSH retains older generations after migration and resumes the highest one.
// Match that selection, rejecting unknown generations and mixed encodings
// instead of silently importing an older transcript or an unloadable root.
func selectedDshGeneration(root *os.Root, path string) (bool, error) {
	entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(filepath.Dir(path)))
	if err != nil {
		return false, err
	}
	latest, selected, encoding := -1, "", ""
	for _, entry := range entries {
		version, ok := dshGeneration(entry.Name())
		if !ok {
			continue
		}
		if version > 3 {
			return false, ErrUnsupported
		}
		if !entry.Type().IsRegular() {
			return false, errors.New("DSH history generation must be a regular file")
		}
		currentEncoding := "jsonl"
		if strings.HasSuffix(entry.Name(), ".zstd") {
			currentEncoding = "zstd"
		}
		if encoding != "" && encoding != currentEncoding {
			return false, errors.New("DSH history contains incompatible physical encodings")
		}
		encoding = currentEncoding
		if version > latest {
			latest, selected = version, entry.Name()
		}
	}
	return filepath.Base(path) == selected, nil
}
