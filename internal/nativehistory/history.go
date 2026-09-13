// Package nativehistory discovers and snapshots selected native agent history.
// It never starts an agent or copies the operator's executable configuration.
package nativehistory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrUnsupported = errors.New("native session history format is unsupported")
var ErrChanged = errors.New("native session changed; refresh the session list before importing")

const MaxSnapshotBytes int64 = 256 << 20
const maxEntries = 2000

type Source struct {
	Harness string `json:"harness"`
	Home    string `json:"home"`
}

type Entry struct {
	NativeID   string    `json:"native_id"`
	Harness    string    `json:"harness"`
	SourceHome string    `json:"source_home"`
	Workdir    string    `json:"workdir"`
	Title      string    `json:"title,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	Revision   string    `json:"revision"`
	path       string
	directory  bool
}

// Reference identifies one immutable import and the native session it resumes.
// The importing node owns the runtime; source histories are never rewritten.
type Reference struct {
	ID            string    `json:"id"`
	Harness       string    `json:"harness"`
	NativeID      string    `json:"native_id"`
	SourceHome    string    `json:"source_home"`
	SourceWorkdir string    `json:"source_workdir"`
	Revision      string    `json:"revision"`
	Digest        string    `json:"digest"`
	ImportedAt    time.Time `json:"imported_at"`
}

func sourceRoot(source Source) (*os.Root, error) {
	if !filepath.IsAbs(source.Home) {
		return nil, errors.New("native history home must be an absolute path")
	}
	switch source.Harness {
	case "codex", "claude-code", "grok":
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, source.Harness)
	}
	return os.OpenRoot(source.Home)
}

func validNativeID(id string) bool {
	return id != "" && len(id) <= 256 && !strings.HasPrefix(id, "ns_") && !strings.ContainsAny(id, "/\\\x00\r\n") && id != "." && id != ".."
}

func clipped(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 160 {
		return string(runes[:160]) + "…"
	}
	return string(runes)
}
