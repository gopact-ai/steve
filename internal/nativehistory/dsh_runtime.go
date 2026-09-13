package nativehistory

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// PrepareDshRuntime converts only an unpublished execution copy to the shipped
// ACP profile's Zstandard encoding. The immutable snapshot keeps original bytes.
// DSH requires the session header to occupy its own complete Zstandard frame.
func PrepareDshRuntime(ctx context.Context, home string) error {
	return filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !isDshTranscript(d.Name()) || strings.HasSuffix(path, ".zstd") {
			return nil
		}
		return compressDshTranscript(ctx, path)
	})
}

func compressDshTranscript(ctx context.Context, path string) error {
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	reader := bufio.NewReaderSize(&contextReader{ctx: ctx, reader: io.LimitReader(input, MaxSnapshotBytes+1)}, 1<<20)
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return err
	}
	out, err := os.OpenFile(path+".zstd", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return err
	}
	defer encoder.Close()
	if _, err := out.Write(encoder.EncodeAll(header, nil)); err != nil {
		return err
	}
	headerBytes := int64(len(header))
	encoder.Reset(out)
	n, err := io.Copy(encoder, reader)
	if err != nil {
		return err
	}
	if n+headerBytes > MaxSnapshotBytes {
		return errors.New("DSH transcript exceeds the snapshot limit")
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := input.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}
