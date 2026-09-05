// Package ops defines the local artifact operation contract shared by the hub
// and node. It depends only on the standard library, not on node transport.
package ops

import "time"

// Kind names a fixed operation, never a command line. The authenticated
// hub supplies paths and object IDs; peer grants still permit only blob reads.
type Kind string

const (
	Init          Kind = "init"
	Snapshot      Kind = "snapshot"
	Checkout      Kind = "checkout"
	Has           Kind = "has"
	Bundle        Kind = "bundle"
	Unbundle      Kind = "unbundle"
	Merge         Kind = "merge"
	Apply         Kind = "apply"
	Changed       Kind = "changed"
	Remove        Kind = "remove"
	ListWorktrees Kind = "list_worktrees"
	PathState     Kind = "path_state"
	WritePath     Kind = "write_path"
)

type Limits struct {
	MaxFiles     int64 `json:"max_files,omitempty"`
	MaxBytes     int64 `json:"max_bytes,omitempty"`
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`
}

// Request is one artifact operation. Path is a bundle/removal path, or a
// relative tree entry for recovery. Before is the
// worktree sweep cutoff. LegacyMerge keeps the pre-2.38 merge fallback.
type Request struct {
	Op          Kind      `json:"op"`
	Repo        string    `json:"repo,omitempty"`
	WorkTree    string    `json:"work_tree,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	Parent      string    `json:"parent,omitempty"`
	From        string    `json:"from,omitempty"`
	Base        string    `json:"base,omitempty"`
	Ours        string    `json:"ours,omitempty"`
	Theirs      string    `json:"theirs,omitempty"`
	Message     string    `json:"message,omitempty"`
	Flatten     bool      `json:"flatten,omitempty"`
	Limits      Limits    `json:"limits,omitempty"`
	Have        []string  `json:"have,omitempty"`
	Path        string    `json:"path,omitempty"`
	Before      time.Time `json:"before,omitzero"`
	LegacyMerge bool      `json:"legacy_merge,omitempty"`
}

type Result struct {
	Commit  string   `json:"commit,omitempty"`
	Changed bool     `json:"changed,omitempty"`
	Has     bool     `json:"has,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	State   string   `json:"state,omitempty"`
}

// Failure is the serializable form of an operation error. Callers use its
// code and fields, never diagnostic output, to identify the failure.
type Failure struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Command  string   `json:"command,omitempty"`
	ExitCode int      `json:"exit_code,omitempty"`
	Stderr   string   `json:"stderr,omitempty"`
	Which    string   `json:"which,omitempty"`
	Have     int64    `json:"have,omitempty"`
	Limit    int64    `json:"limit,omitempty"`
	Paths    []string `json:"paths,omitempty"`
}

func (e *Failure) Error() string { return e.Message }
