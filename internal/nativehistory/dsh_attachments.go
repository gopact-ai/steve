package nativehistory

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	switch name {
	case "session.jsonl", "session.jsonl.zstd", "session.v3.jsonl", "session.v3.jsonl.zstd":
		return true
	default:
		return false
	}
}
