package nativehistory

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// DSH stores the immutable session header as the first independently compressed
// Zstandard frame, followed by event frames. Read only bounded header data.
func readDshEntry(entry Entry, input io.Reader) (Entry, error) {
	var reader io.Reader = io.LimitReader(input, 8<<20)
	if strings.HasSuffix(entry.path, ".zstd") {
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderMaxWindow(8<<20))
		if err != nil {
			return entry, err
		}
		defer decoder.Close()
		reader = decoder
	}
	scan := bufio.NewScanner(io.LimitReader(reader, 1<<20))
	scan.Buffer(make([]byte, 4096), 1<<20)
	if !scan.Scan() {
		return entry, errors.Join(errors.New("DSH session header is missing"), scan.Err())
	}
	var header struct {
		Type            string `json:"type"`
		ID              string `json:"id"`
		Cwd             string `json:"cwd"`
		Origin          string `json:"origin"`
		DelegationDepth int    `json:"delegationDepth"`
	}
	if err := json.Unmarshal(scan.Bytes(), &header); err != nil {
		return entry, err
	}
	if header.Type != "session" {
		return entry, errors.New("unrecognized DSH session header")
	}
	// DSH's ACP resume surface accepts persisted root sessions only.
	if header.Origin == "subagent" || header.DelegationDepth > 0 {
		return entry, nil
	}
	entry.NativeID, entry.Workdir = header.ID, header.Cwd
	entry.path = filepath.Dir(entry.path)
	return entry, nil
}
