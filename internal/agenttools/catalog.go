// Package agenttools describes the supported installed tools without loading
// application configuration, credentials or agent processes.
package agenttools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

var (
	ErrUnsupported = errors.New("unsupported agent tool")
	ErrUnavailable = errors.New("agent tool is unavailable")
	ErrConfigured  = errors.New("agent tool already has different settings")
)

type Candidate struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Harness    string   `json:"harness"`
	Executable string   `json:"executable,omitempty"`
	Adapter    string   `json:"adapter,omitempty"`
	Installed  bool     `json:"installed"`
	Requires   []string `json:"requires,omitempty"`
	Configured bool     `json:"configured"`
	Registered bool     `json:"registered"`
}

type Discovery struct {
	Revision string      `json:"revision"`
	Agents   []Candidate `json:"agents"`
}

type EnrollRequest struct {
	CandidateID      string `json:"candidate_id"`
	AgentID          string `json:"agent_id"`
	ExpectedRevision string `json:"expected_revision"`
}

// InstallRequest crosses the node transport. Commands, paths, environment and
// permissions are deliberately absent; they belong to the selected machine.
type InstallRequest struct {
	CandidateID      string `json:"candidate_id"`
	ExpectedRevision string `json:"expected_revision"`
}

type Enrollment struct {
	CandidateID string `json:"candidate_id"`
	AgentID     string `json:"agent_id,omitempty"`
	Harness     string `json:"harness"`
	Revision    string `json:"revision"`
	Registered  bool   `json:"registered"`
}

type Options struct{ Path, HomeDir string }

var supported = []Candidate{
	{ID: "codex", Name: "Codex", Harness: "codex", Adapter: "codex-acp"},
	{ID: "claude", Name: "Claude Code", Harness: "claude-code", Adapter: "claude-agent-acp"},
	{ID: "grok", Name: "Grok", Harness: "grok"},
	{ID: "kimi", Name: "Kimi Code", Harness: "kimi"},
}

func Catalog() []Candidate { return slices.Clone(supported) }

func Lookup(id string) (Candidate, bool) {
	for _, candidate := range supported {
		if candidate.ID == id {
			return candidate, true
		}
	}
	return Candidate{}, false
}

// FromPaths maps a machine's already checked executable paths to the shared
// catalog. It performs no local filesystem access and is suitable for SSH
// discovery output, which remains advisory until the node checks it again.
func FromPaths(paths map[string]string) []Candidate {
	out := Catalog()
	for i := range out {
		candidate := &out[i]
		if path := paths[candidate.ID]; usablePath(path) {
			candidate.Executable = path
			candidate.Installed = true
		}
		if candidate.Installed && candidate.Adapter != "" {
			for _, name := range []string{"node", "npm"} {
				if !usablePath(paths[name]) {
					candidate.Requires = append(candidate.Requires, name)
				}
			}
		}
	}
	return out
}

func usablePath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\r\n")
}

// Discover checks executable files only; it never runs a discovered program.
func Discover(options Options) []Candidate {
	search := options.Path
	if search == "" {
		search = ExecutablePath(options.HomeDir)
	}
	paths := make(map[string]string)
	for _, candidate := range supported {
		paths[candidate.ID] = findExecutable(search, candidate.ID)
	}
	for _, name := range []string{"node", "npm"} {
		paths[name] = findExecutable(search, name)
	}
	return FromPaths(paths)
}

func findExecutable(search, name string) string {
	for _, dir := range filepath.SplitList(search) {
		if !filepath.IsAbs(dir) {
			continue
		}
		path := filepath.Join(dir, name)
		if Executable(path) {
			return path
		}
	}
	return ""
}

func Executable(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

type Declaration struct {
	Harness, Adapter, Command string
	Args                      []string
}

func Registration(candidate Candidate) (Declaration, error) {
	canonical, ok := Lookup(candidate.ID)
	if !ok || canonical.Harness != candidate.Harness || canonical.Adapter != candidate.Adapter {
		return Declaration{}, ErrUnsupported
	}
	if !candidate.Installed || !Executable(candidate.Executable) {
		return Declaration{}, fmt.Errorf("%w: %s changed; refresh discovery", ErrUnavailable, canonical.Name)
	}
	if len(candidate.Requires) > 0 {
		return Declaration{}, fmt.Errorf("%w: %s needs %s", ErrUnavailable, canonical.Name, strings.Join(candidate.Requires, ", "))
	}
	out := Declaration{Harness: canonical.Harness, Adapter: canonical.Adapter}
	if canonical.Adapter == "" {
		out.Command = candidate.Executable
		switch candidate.ID {
		case "grok":
			out.Args = []string{"agent", "--no-leader", "stdio"}
		case "kimi":
			out.Args = []string{"acp"}
		}
	}
	return out, nil
}
