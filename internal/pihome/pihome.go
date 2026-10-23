// Package pihome prepares and checks the isolated state directories Steve
// gives pi (through the pi-acp adapter).
//
// pi asks for no approval before a tool call, and pi-acp delegates neither
// file access nor terminals to its ACP client, so Steve's permission broker
// never sees what pi reads, writes or runs. The only control is which tools
// pi loads. Steve therefore keeps two homes per installation:
//
//   - full: pi's own default tools (read, bash, edit, write);
//   - read-only: only read, grep, find and ls — nothing that writes a file or
//     runs a command — with project-supplied settings and extensions
//     disabled, so a working folder cannot add tools back.
//
// A policy that only reads (read, deny) runs pi from the read-only home;
// anything else from the full one. Each home records what it enforces, and
// Select refuses to start pi under a read-only policy from a home that does
// not enforce it, such as an operator's own PI_CODING_AGENT_DIR.
package pihome

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// EnvAgentDir is pi's state directory.
	EnvAgentDir = "PI_CODING_AGENT_DIR"
	// EnvCommand is the pi executable pi-acp starts.
	EnvCommand = "PI_ACP_PI_COMMAND"
	// EnvHomes names the directory holding Steve's two pi homes. Only Steve
	// sets it; a launch without it uses PI_CODING_AGENT_DIR as given.
	EnvHomes = "STEVE_PI_HOMES"

	Full     = "full"
	ReadOnly = "read-only"

	marker = "steve-enforced.json"
)

// ReadOnlyTools are the pi built-in tools that neither write nor execute.
var ReadOnlyTools = []string{"read", "grep", "find", "ls"}

// ErrUnenforced is a read-only policy for a pi home that does not restrict
// pi's tools.
var ErrUnenforced = errors.New("pi cannot honour a read-only permission policy here")

// Class is the home a permission policy needs: read and deny only read.
func Class(policy string) string {
	switch policy {
	case "", "read", "deny":
		return ReadOnly
	}
	return Full
}

// modelAccessKeys are the settings.json keys that choose the model and its
// thinking level. Extensions, packages, skill paths, themes and project
// trust stay the operator's own terminal.
var modelAccessKeys = []string{"defaultProvider", "defaultModel", "defaultThinkingLevel", "enabledModels", "modelThinkingLevels"}

// Prepare writes both homes under base from the operator's pi directory
// source ("" for none). Credentials (auth.json) and provider definitions
// (models.json, which may hold API keys or the commands that fetch them)
// are linked, never copied, so a later login or key rotation is seen and no
// second copy of a secret is written. Both homes share base/skills, the one
// directory Steve materializes skills into.
func Prepare(base, source string) error {
	if err := os.MkdirAll(filepath.Join(base, "skills"), 0o700); err != nil {
		return fmt.Errorf("create pi runtime: %w", err)
	}
	for _, class := range []string{Full, ReadOnly} {
		if err := PrepareHome(filepath.Join(base, class), source, class); err != nil {
			return err
		}
	}
	return nil
}

// Dir is the home of one class under base.
func Dir(base, class string) string { return filepath.Join(base, class) }

// PrepareHome writes one home of the given class; its skills directory is
// the shared one beside it.
func PrepareHome(dest, source, class string) error {
	if class != Full && class != ReadOnly {
		return fmt.Errorf("pi home class %q", class)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("create pi runtime: %w", err)
	}
	skills := filepath.Join(dest, "skills")
	if info, err := os.Lstat(skills); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("pi runtime %s holds its own skills directory", dest)
	}
	_ = os.Remove(skills)
	if err := os.Symlink(filepath.Join("..", "skills"), skills); err != nil {
		return fmt.Errorf("link pi skills: %w", err)
	}
	var raw []byte
	if source != "" {
		for _, name := range []string{"auth.json", "models.json"} {
			if err := link(dest, source, name); err != nil {
				return err
			}
		}
		data, err := os.ReadFile(filepath.Join(source, "settings.json"))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read pi settings: %w", err)
		}
		raw = data
	}
	if err := writeFile(filepath.Join(dest, "settings.json"), Settings(raw, class)); err != nil {
		return fmt.Errorf("write isolated pi settings: %w", err)
	}
	record, _ := json.Marshal(map[string]any{"class": class, "tools": toolsFor(class)})
	return writeFile(filepath.Join(dest, marker), append(record, '\n'))
}

// Settings keeps the operator's model choice and sets what Steve enforces.
func Settings(src []byte, class string) []byte {
	var parsed map[string]json.RawMessage
	_ = json.Unmarshal(src, &parsed)
	out := map[string]any{}
	for _, key := range modelAccessKeys {
		if value, ok := parsed[key]; ok && string(bytes.TrimSpace(value)) != "null" {
			out[key] = value
		}
	}
	// Steve's turns have no terminal to answer pi's prompts; a working
	// folder's .pi settings, MCP servers and extensions never load.
	out["defaultProjectTrust"] = "never"
	out["quietStartup"] = true
	if tools := toolsFor(class); tools != nil {
		out["defaultTools"] = tools
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return []byte("{}\n")
	}
	return append(data, '\n')
}

func toolsFor(class string) []string {
	if class == ReadOnly {
		return append([]string(nil), ReadOnlyTools...)
	}
	return nil // pi's defaults
}

// Enforced reports the class a home was prepared as, or "" for a directory
// Steve did not prepare.
func Enforced(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, marker))
	if err != nil {
		return ""
	}
	var record struct {
		Class string `json:"class"`
	}
	if json.Unmarshal(raw, &record) != nil {
		return ""
	}
	return record.Class
}

// Select returns env with pi's home chosen for policy. Steve's own homes
// are switched by class; any other directory must already enforce what a
// read-only policy needs, or pi does not start.
func Select(env []string, policy string) ([]string, error) {
	class := Class(policy)
	dir := lookup(env, EnvAgentDir)
	if base := lookup(env, EnvHomes); base != "" {
		dir = filepath.Join(base, class)
		env = replace(env, EnvAgentDir, dir)
	}
	if class == Full {
		return env, nil
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".pi", "agent")
	}
	if Enforced(dir) != ReadOnly {
		return nil, fmt.Errorf("%w: %s does not restrict pi to read-only tools; let Steve manage pi's home or use a write policy", ErrUnenforced, dir)
	}
	return env, nil
}

func lookup(env []string, key string) string {
	value := ""
	for _, item := range env {
		if v, ok := strings.CutPrefix(item, key+"="); ok {
			value = v
		}
	}
	return value
}

func replace(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, key+"=") {
			out = append(out, item)
		}
	}
	return append(out, key+"="+value)
}

func link(dest, source, name string) error {
	src := filepath.Join(source, name)
	if _, err := os.Stat(src); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	path := filepath.Join(dest, name)
	_ = os.Remove(path)
	if err := os.Symlink(src, path); err != nil {
		return fmt.Errorf("link %s: %w", name, err)
	}
	return nil
}

func writeFile(path string, data []byte) error {
	// A link left in the way would write into the operator's own file.
	_ = os.Remove(path)
	return os.WriteFile(path, data, 0o600)
}
