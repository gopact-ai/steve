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
// Every pi start goes through Launch, which re-prepares the home its policy
// needs, checks it, and starts pi through a wrapper in that home. The
// wrapper passes --no-extensions always and, in the read-only home, an
// explicit --tools allowlist: pi then ignores the tool list a resumed
// transcript recorded. It also refuses to resume a session file kept in the
// other home. Steve records which home opened each session (Remember) and
// starts afresh instead of resuming one opened under the other (Resumable).
//
// A launch without Steve's homes, such as an operator's own
// PI_CODING_AGENT_DIR, is refused: the policy always decides the home.
//
// The homes are ordinary directories of the user Steve runs as. Another
// process of that user, a full-class pi included, can change them between
// starts; Launch rewrites and checks them on every start but cannot hold
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
	"sync"
)

const (
	// EnvAgentDir is pi's state directory.
	EnvAgentDir = "PI_CODING_AGENT_DIR"
	// EnvCommand is the pi executable pi-acp starts.
	EnvCommand = "PI_ACP_PI_COMMAND"
	// EnvHomes names the directory holding Steve's two pi homes.
	EnvHomes = "STEVE_PI_HOMES"
	// EnvReal is the pi executable the wrapper runs; Launch moves the
	// registered PI_ACP_PI_COMMAND here.
	EnvReal = "STEVE_PI_REAL"

	Full     = "full"
	ReadOnly = "read-only"

	marker  = "steve-enforced.json"
	wrapper = "steve-pi"
	source  = "source"
	classes = "classes.json"
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
	// Remembered so every launch can prepare its home again from the same
	// operator directory.
	if err := writeFile(filepath.Join(base, source), []byte(operator+"\n")); err != nil {
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

// PrepareHome writes one home of the given class; its skills directory is
// the shared one beside it.
func PrepareHome(dest, source, class string) error {
	if class != Full && class != ReadOnly {
		return fmt.Errorf("pi home class %q", class)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("create pi runtime: %w", err)
	}
	// pi loads <home>/extensions/* without asking; nothing belongs there.
	if err := os.RemoveAll(filepath.Join(dest, "extensions")); err != nil {
		return fmt.Errorf("clear pi extensions: %w", err)
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
	if err := writeExecutable(filepath.Join(dest, wrapper), Wrapper(class)); err != nil {
		return fmt.Errorf("write pi launcher: %w", err)
	}
	record, _ := json.Marshal(map[string]any{"class": class, "tools": toolsFor(class)})
	return writeFile(filepath.Join(dest, marker), append(record, '\n'))
}

// Wrapper is the launcher pi-acp starts instead of pi. It runs the real pi
// (STEVE_PI_REAL) without extensions, in the read-only home with an
// explicit tool allowlist, and refuses a session file kept outside this
// home: one opened under the other policy.
func Wrapper(class string) []byte {
	tools := ""
	if class == ReadOnly {
		tools = " --tools " + strings.Join(ReadOnlyTools, ",")
	}
	return []byte(`#!/bin/sh
# generated by steve: pi under the ` + class + ` permission home
real="${STEVE_PI_REAL:-pi}"
home="${PI_CODING_AGENT_DIR:?}"
previous=""
for arg in "$@"; do
  if [ "$previous" = "--session" ]; then
    case "$arg" in
      "$home"/sessions/*) ;;
      *) echo "steve: pi session $arg was opened under another permission policy" >&2; exit 3 ;;
    esac
  fi
  previous="$arg"
done
exec "$real" "$@" --no-extensions` + tools + "\n")
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

// Enforced reports the class a home holds, or "" when its contents do not
// match a class Steve prepared: the marker, the settings' tool list and
// project trust, the launcher, and an empty extensions directory are all
// checked, not just the marker.
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
	if entries, err := os.ReadDir(filepath.Join(dir, "extensions")); err == nil && len(entries) > 0 || err != nil && !os.IsNotExist(err) {
		return ""
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

// Launch fits a pi start to its policy: it prepares the home the policy
// needs again (settings, launcher, no extensions), checks it, points
// PI_CODING_AGENT_DIR at it and has pi-acp start pi through that home's
// launcher. A start that does not carry Steve's homes is refused whatever
// PI_CODING_AGENT_DIR says: the policy decides the home, never the
// environment.
func Launch(env []string, policy string) ([]string, error) {
	class := Class(policy)
	base := lookup(env, EnvHomes)
	if base == "" {
		return nil, fmt.Errorf("%w: this pi start has no Steve-managed home (an own PI_CODING_AGENT_DIR is not used)", ErrUnenforced)
	}
	dir := Dir(base, class)
	mu := lockFor(dir)
	mu.Lock()
	defer mu.Unlock()
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

var locks sync.Map

// operatorDir is the operator's pi directory the homes under base were
// prepared from. Homes prepared before Steve recorded it, or by a plugin
// profile, use ~/.pi/agent, the directory runtime isolation seeds from.
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

func lockFor(dir string) *sync.Mutex {
	mu, _ := locks.LoadOrStore(dir, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// Remember records which home a pi session was opened under.
func Remember(env []string, session, policy string) error {
	base := lookup(env, EnvHomes)
	if base == "" || session == "" {
		return nil
	}
	mu := lockFor(base)
	mu.Lock()
	defer mu.Unlock()
	known := readClasses(base)
	if known[session] == Class(policy) {
		return nil
	}
	known[session] = Class(policy)
	data, err := json.MarshalIndent(known, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	return writeFile(filepath.Join(base, classes), append(data, '\n'))
}

// Resumable reports whether a pi session may be resumed under policy: one
// Steve recorded under the other home must start afresh instead. A session
// Steve has no record of is resumable; the launcher still refuses its file
// if it lives in the other home.
func Resumable(env []string, session, policy string) bool {
	base := lookup(env, EnvHomes)
	if base == "" || session == "" {
		return true
	}
	mu := lockFor(base)
	mu.Lock()
	defer mu.Unlock()
	recorded, ok := readClasses(base)[session]
	return !ok || recorded == Class(policy)
}

func readClasses(base string) map[string]string {
	known := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(base, classes))
	if err == nil {
		_ = json.Unmarshal(raw, &known)
	}
	return known
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

func writeExecutable(path string, data []byte) error {
	_ = os.Remove(path)
	return os.WriteFile(path, data, 0o700)
}
