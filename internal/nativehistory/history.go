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
	if !Importable(source.Harness) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, source.Harness)
	}
	return os.OpenRoot(source.Home)
}

// Importable reports whether Steve reads this harness's native session
// history. pi keeps its own sessions, which Steve does not import.
func Importable(harness string) bool { return harness == "codex" || harness == "claude-code" }

func validNativeID(id string) bool {
	return id != "" && len(id) <= 256 && !strings.HasPrefix(id, "ns_") && !strings.ContainsAny(id, "/\\\x00\r\n") && id != "." && id != ".."
}

func validSourceNativeID(_, id string) bool { return validNativeID(id) }

func clipped(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 160 {
		return string(runes[:160]) + "…"
	}
	return string(runes)
}
