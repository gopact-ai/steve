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
	"unicode"
	"unicode/utf8"
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

func (r *Reference) Clone() *Reference {
	if r == nil {
		return nil
	}
	copy := *r
	return &copy
}

// Validate fixes the imported history to its original workspace. Moving it to
// another project requires an explicit provider-specific remapping contract.
func (r Reference) Validate(harness, workdir string) error {
	if !validReferenceID(r.ID) || !validSourceNativeID(r.Harness, r.NativeID) || !filepath.IsAbs(r.SourceHome) || !filepath.IsAbs(r.SourceWorkdir) || r.Digest == "" || r.Revision == "" || r.ImportedAt.IsZero() {
		return errors.New("invalid native history reference")
	}
	if r.Harness != harness || !filepath.IsAbs(workdir) || filepath.Clean(r.SourceWorkdir) != filepath.Clean(workdir) {
		return errors.New("native history requires its selected harness and original workspace")
	}
	return nil
}

func sourceRoot(source Source) (*os.Root, error) {
	if !filepath.IsAbs(source.Home) {
		return nil, errors.New("native history home must be an absolute path")
	}
	switch source.Harness {
	case "codex", "claude-code", "grok", "dsh":
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, source.Harness)
	}
	return os.OpenRoot(source.Home)
}

func validNativeID(id string) bool {
	return id != "" && len(id) <= 256 && !strings.HasPrefix(id, "ns_") && !strings.ContainsAny(id, "/\\\x00\r\n") && id != "." && id != ".."
}

func validSourceNativeID(harness, id string) bool {
	if harness != "dsh" {
		return validNativeID(id)
	}
	// DSH encodes opaque IDs into safe filenames itself. Chat/thread IDs
	// can contain slashes; Steve selects them by metadata, never as paths.
	return id != "" && id != "." && id != ".." && len(id) <= 256 && utf8.ValidString(id) && strings.IndexFunc(id, unicode.IsControl) < 0
}

func clipped(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 160 {
		return string(runes[:160]) + "…"
	}
	return string(runes)
}
