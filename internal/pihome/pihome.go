// Package pihome prepares and checks the isolated state directories Steve
// gives pi (through the pi-acp adapter).
//
// pi asks for no approval before a tool call, and pi-acp delegates neither
// file access nor terminals to its ACP client, so Steve's permission broker
// never sees what pi reads, writes or runs. The only control is which tools
// the pi process loads. Steve therefore keeps two homes per installation:
//
//   - full: pi's own default tools (read, bash, edit, write);
//   - read-only: only read, grep, find and ls, nothing that writes a file or
//     runs a command.
//
// Every pi start goes through Launch, which brings the home its policy
// needs back to what Steve prepared, checks it, and starts pi through a
// launcher in that home. The launcher passes --no-extensions always and, in
// the read-only home, an explicit --tools allowlist: pi then ignores the
// tool list a resumed transcript recorded. It also refuses to resume a
// session file that does not resolve into its own home. Steve records which
// home opened each session (Remember) and starts afresh instead of resuming
// one opened under the other (Resumable, Refused).
//
// A launch without Steve's homes, such as an operator's own
// PI_CODING_AGENT_DIR, is refused: the policy always decides the home.
//
// The homes are ordinary directories of the user Steve runs as. Another
// process of that user, a full-class pi included, can change them between
// starts; Launch restores and checks them on every start but cannot hold
// them after pi has started.
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
	// EnvHomes names the directory holding Steve's two pi homes.
	EnvHomes = "STEVE_PI_HOMES"
	// EnvReal is the pi executable the launcher runs; Launch moves the
	// registered PI_ACP_PI_COMMAND here.
	EnvReal = "STEVE_PI_REAL"

	Full     = "full"
	ReadOnly = "read-only"

	marker  = "steve-enforced.json"
	wrapper = "steve-pi"
	source  = "source"
	refused = "refused"
	tmpName = ".steve-tmp-"
)

// ReadOnlyTools are the pi built-in tools that neither write nor execute.
var ReadOnlyTools = []string{"read", "grep", "find", "ls"}

// ErrUnenforced is a pi start whose permission policy Steve cannot hold:
// no Steve-prepared home, or one whose contents were changed.
var ErrUnenforced = errors.New("pi cannot honour its permission policy here")

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

// kept are the entries a home may hold. pi reads others from its directory
// without asking (mcp.json, trust.json, extensions/, tools/, themes/,
// prompts/, SYSTEM.md, AGENTS.md, npm/ and git/ packages, keybindings), so
// Launch removes them and Enforced refuses a home that has them. What pi
// writes while it runs and never loads as configuration is kept: sessions,
// the model store, crash records, scratch space and its lock files.
var kept = map[string]bool{
	"settings.json": true, "auth.json": true, "models.json": true, "skills": true,
	wrapper: true, marker: true, refused: true,
	"sessions": true, "models-store.json": true, "crashes.json": true, "tmp": true,
}

func keptEntry(name string) bool {
	return kept[name] || strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, tmpName)
}

// Prepare writes both homes under base from the operator's pi directory
// source ("" for none). Credentials (auth.json) and provider definitions
// (models.json, which may hold API keys or the commands that fetch them)
// are linked, never copied, so a later login or key rotation is seen and no
// second copy of a secret is written. Both homes share base/skills, the one
// directory Steve materializes skills into.
func Prepare(base, operator string) error {
	if err := os.MkdirAll(filepath.Join(base, "skills"), 0o700); err != nil {
		return fmt.Errorf("create pi runtime: %w", err)
	}
	unlock, err := lockBase(base)
	if err != nil {
		return err
	}
	defer unlock()
	// Remembered so every launch can prepare its home again from the same
	// operator directory.
	if err := writeAtomic(filepath.Join(base, source), []byte(operator+"\n"), 0o600); err != nil {
		return fmt.Errorf("record pi source: %w", err)
	}
	for _, class := range []string{Full, ReadOnly} {
		if err := PrepareHome(filepath.Join(base, class), operator, class); err != nil {
			return err
		}
	}
	return nil
}

// Dir is the home of one class under base.
func Dir(base, class string) string { return filepath.Join(base, class) }

// PrepareHome brings one home of the given class to what Steve prepares;
// its skills directory is the shared one beside it. Files are replaced by
// rename and left alone when already right, so a pi starting from the same
// home at that moment never finds one missing.
func PrepareHome(dest, source, class string) error {
	if class != Full && class != ReadOnly {
		return fmt.Errorf("pi home class %q", class)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("create pi runtime: %w", err)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return fmt.Errorf("read pi runtime: %w", err)
	}
	for _, entry := range entries {
		if !keptEntry(entry.Name()) {
			if err := os.RemoveAll(filepath.Join(dest, entry.Name())); err != nil {
				return fmt.Errorf("clear pi runtime %s: %w", entry.Name(), err)
			}
		}
	}
	skills := filepath.Join(dest, "skills")
	if info, err := os.Lstat(skills); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("pi runtime %s holds its own skills directory", dest)
	}
	if err := linkAtomic(skills, filepath.Join("..", "skills")); err != nil {
		return fmt.Errorf("link pi skills: %w", err)
	}
	var raw []byte
	if source != "" {
		for _, name := range []string{"auth.json", "models.json"} {
			if err := linkAtomic(filepath.Join(dest, name), filepath.Join(source, name)); err != nil {
				return fmt.Errorf("link %s: %w", name, err)
			}
		}
		data, err := os.ReadFile(filepath.Join(source, "settings.json"))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read pi settings: %w", err)
		}
		raw = data
	}
	if err := writeAtomic(filepath.Join(dest, "settings.json"), Settings(raw, class), 0o600); err != nil {
		return fmt.Errorf("write isolated pi settings: %w", err)
	}
	if err := writeAtomic(filepath.Join(dest, wrapper), Wrapper(class), 0o700); err != nil {
		return fmt.Errorf("write pi launcher: %w", err)
	}
	return writeAtomic(filepath.Join(dest, marker), markerFor(class), 0o600)
}

func markerFor(class string) []byte {
	record, _ := json.Marshal(map[string]any{"class": class, "tools": toolsFor(class)})
	return append(record, '\n')
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
	// Steve's turns have no terminal to answer pi's prompts; pi loads no
	// working folder's .pi settings, MCP servers or extensions.
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

// Enforced reports the class a home holds, or "" when its contents do not
// match a class Steve prepared: the marker, the settings' tool list and
// project trust, the launcher, the shared skills link, and no entry pi
// would load as configuration (extensions, mcp.json, trust.json and the
// rest outside kept).
func Enforced(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, marker))
	if err != nil {
		return ""
	}
	var record struct {
		Class string `json:"class"`
	}
	if json.Unmarshal(raw, &record) != nil || (record.Class != Full && record.Class != ReadOnly) {
		return ""
	}
	class := record.Class
	settings, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return ""
	}
	var parsed struct {
		DefaultTools *[]string `json:"defaultTools"`
		Trust        string    `json:"defaultProjectTrust"`
	}
	if json.Unmarshal(settings, &parsed) != nil || parsed.Trust != "never" {
		return ""
	}
	if want := toolsFor(class); want == nil && parsed.DefaultTools != nil || want != nil && (parsed.DefaultTools == nil || !equal(*parsed.DefaultTools, want)) {
		return ""
	}
	launcher, err := os.ReadFile(filepath.Join(dir, wrapper))
	if err != nil || !bytes.Equal(launcher, Wrapper(class)) {
		return ""
	}
	if target, err := os.Readlink(filepath.Join(dir, "skills")); err != nil || target != filepath.Join("..", "skills") {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !keptEntry(entry.Name()) {
			return ""
		}
	}
	return class
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Launch fits a pi start to its policy: it brings the home the policy needs
// back to what Steve prepares, checks it, points PI_CODING_AGENT_DIR at it
// and has pi-acp start pi through that home's launcher. A start that does
// not carry Steve's homes is refused whatever PI_CODING_AGENT_DIR says: the
// policy decides the home, never the environment.
func Launch(env []string, policy string) ([]string, error) {
	class := Class(policy)
	base := lookup(env, EnvHomes)
	if base == "" {
		return nil, fmt.Errorf("%w: this pi start has no Steve-managed home (an own PI_CODING_AGENT_DIR is not used)", ErrUnenforced)
	}
	dir := Dir(base, class)
	unlock, err := lockBase(base)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := PrepareHome(dir, operatorDir(base), class); err != nil {
		return nil, err
	}
	if Enforced(dir) != class {
		return nil, fmt.Errorf("%w: %s does not hold the %s home", ErrUnenforced, dir, class)
	}
	real := lookup(env, EnvReal)
	if real == "" {
		real = lookup(env, EnvCommand)
	}
	if real == "" {
		real = "pi"
	}
	env = replace(env, EnvAgentDir, dir)
	env = replace(env, EnvReal, real)
	return replace(env, EnvCommand, filepath.Join(dir, wrapper)), nil
}

// operatorDir is the operator's pi directory the homes under base were
// prepared from. Homes prepared before Steve recorded it use ~/.pi/agent,
// the directory runtime isolation seeds from.
func operatorDir(base string) string {
	if raw, err := os.ReadFile(filepath.Join(base, source)); err == nil {
		return strings.TrimSpace(string(raw))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".pi", "agent")
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
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
