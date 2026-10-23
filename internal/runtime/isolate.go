// Package runtime prepares isolated harness homes so Steve does not inherit
// the operator's IDE skill catalog.
package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/pihome"
)

func CodexHome(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", harness.Codex)
}

func ClaudeHome(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", harness.ClaudeCode)
}

// PiHome holds Steve's two pi homes and the skills they share; see
// internal/pihome.
func PiHome(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", harness.Pi)
}

func Prepare(stateDir string) error {
	return PrepareSelected(stateDir, []string{harness.Codex, harness.ClaudeCode, harness.Pi})
}

// PrepareSelected prepares only the tools the user registered. An empty
// selection does not read tool settings, link credentials or create runtimes.
// Custom harnesses manage their own runtime and are left untouched.
func PrepareSelected(stateDir string, selected []string) error {
	if len(selected) == 0 {
		return nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		userHome = ""
	}
	source := func(dir string) string {
		if userHome == "" {
			return ""
		}
		return filepath.Join(userHome, dir)
	}
	seen := make(map[string]bool, len(selected))
	for _, id := range selected {
		if seen[id] {
			continue
		}
		seen[id] = true
		var err error
		switch id {
		case harness.Codex:
			err = PrepareCodex(CodexHome(stateDir), source(".codex"))
		case harness.ClaudeCode:
			err = PrepareClaude(ClaudeHome(stateDir), source(".claude"))
		case harness.Pi:
			err = PreparePi(PiHome(stateDir), source(filepath.Join(".pi", "agent")))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func SkillDests(stateDir string) []string {
	return SelectedSkillDests(stateDir, []string{harness.Codex, harness.ClaudeCode, harness.Pi})
}

// SelectedSkillDests returns each selected built-in tool's skills directory.
func SelectedSkillDests(stateDir string, selected []string) []string {
	dests := make([]string, 0, len(selected))
	seen := make(map[string]bool, len(selected))
	for _, id := range selected {
		if seen[id] {
			continue
		}
		seen[id] = true
		var dest string
		switch id {
		case harness.Codex:
			dest = CodexHome(stateDir)
		case harness.ClaudeCode:
			dest = ClaudeHome(stateDir)
		case harness.Pi:
			dest = PiHome(stateDir)
		}
		if dest != "" {
			dests = append(dests, filepath.Join(dest, "skills"))
		}
	}
	return dests
}

func ApplyEnv(env []string, harnessID, stateDir string) []string {
	key, value := "", ""
	switch harnessID {
	case harness.Codex:
		key, value = harness.EnvCodexHome, CodexHome(stateDir)
	case harness.ClaudeCode:
		key, value = harness.EnvClaudeConfigDir, ClaudeHome(stateDir)
	case harness.Pi:
		// An operator's own PI_CODING_AGENT_DIR is theirs to keep; Steve then
		// starts pi under a read-only policy only if that home enforces it.
		if HasEnv(env, harness.EnvPiAgentDir) {
			return env
		}
		// Until a launch knows its policy the read-only home is the one in
		// force: a start that never chooses runs without write tools.
		out := slices.Clone(env)
		out = append(out, pihome.EnvHomes+"="+PiHome(stateDir))
		return append(out, harness.EnvPiAgentDir+"="+pihome.Dir(PiHome(stateDir), pihome.ReadOnly))
	default:
		return env
	}
	if HasEnv(env, key) {
		return env
	}
	out := make([]string, len(env), len(env)+1)
	copy(out, env)
	return append(out, key+"="+value)
}

func PrepareCodex(dest, userCodex string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("create codex runtime: %w", err)
	}
	var raw []byte
	if userCodex != "" {
		if err := linkAuth(dest, userCodex, "auth.json"); err != nil {
			return err
		}
		src := filepath.Join(userCodex, "config.toml")
		data, err := os.ReadFile(src)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read codex config: %w", err)
		}
		raw = data
	}
	filtered := FilterCodexConfig(raw)
	if err := os.WriteFile(filepath.Join(dest, "config.toml"), filtered, 0o600); err != nil {
		return fmt.Errorf("write isolated codex config: %w", err)
	}
	return os.MkdirAll(filepath.Join(dest, "skills"), 0o700)
}

func PrepareClaude(dest, userClaude string) error {
	if err := os.MkdirAll(filepath.Join(dest, "skills"), 0o700); err != nil {
		return fmt.Errorf("create claude runtime: %w", err)
	}
	if userClaude == "" {
		return nil
	}
	// Credentials are linked, not copied: Claude Code refreshes its OAuth
	// token in place, and a stale copy would expire under the gateway.
	// Without this link the isolated CLAUDE_CONFIG_DIR had no credentials at
	// all, which is exactly the "Authentication required" every gateway
	// claude turn died with.
	if err := linkAuth(dest, userClaude, ".credentials.json"); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(userClaude, "settings.json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read claude settings: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "settings.json"), FilterClaudeSettings(raw), 0o600); err != nil {
		return fmt.Errorf("write isolated claude settings: %w", err)
	}
	return nil
}

// claudeModelAccessKeys are the top-level settings.json keys that say where
// the model endpoint is and which models it offers: env carries base URL,
// custom headers and tokens; modelPicker, availableModels and modelOverrides
// are the catalogue behind that endpoint. Without the catalogue the isolated
// harness offers Claude Code's built-in lineup, which a relay may not serve.
var claudeModelAccessKeys = []string{"env", "modelPicker", "availableModels", "modelOverrides"}

// FilterClaudeSettings keeps the model-access keys of the operator's
// settings.json and nothing else: the default model, effort, permissions,
// hooks and UI preferences stay the operator's own terminal. A key set to
// null is dropped rather than forwarded, because Claude Code validates the
// file and a null catalogue is an error, not an absence.
func FilterClaudeSettings(src []byte) []byte {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(src, &parsed); err != nil {
		return []byte("{}\n")
	}
	kept := make(map[string]json.RawMessage, len(claudeModelAccessKeys))
	for _, key := range claudeModelAccessKeys {
		value, ok := parsed[key]
		if !ok || string(bytes.TrimSpace(value)) == "null" {
			continue
		}
		kept[key] = value
	}
	if len(kept) == 0 {
		return []byte("{}\n")
	}
	out, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return []byte("{}\n")
	}
	return append(out, '\n')
}

// PreparePi writes Steve's two pi homes from the operator's pi directory.
func PreparePi(dest, userPi string) error { return pihome.Prepare(dest, userPi) }

func linkAuth(dest, userHome, name string) error {
	src := filepath.Join(userHome, name)
	if _, err := os.Stat(src); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	// Keep the link even before the first login. Nested runtime homes must
	// observe later login, credential replacement and logout at the source.
	link := filepath.Join(dest, name)
	// Drop the previous link first; not-exist is the usual answer, and any
	// other failure resurfaces from Symlink as an existing path.
	_ = os.Remove(link)
	if err := os.Symlink(src, link); err != nil {
		return fmt.Errorf("link %s: %w", name, err)
	}
	return nil
}

func FilterCodexConfig(src []byte) []byte {
	var out []string
	skipUntilHeader := false
	skipNotify := false
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if skipNotify {
			out = maybeKeep(out, line)
			if strings.Contains(line, "]") {
				skipNotify = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "notify") && strings.Contains(trimmed, "[") {
			skipNotify = !strings.Contains(trimmed, "]")
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			skipUntilHeader = dropCodexTable(trimmed)
		}
		if skipUntilHeader {
			continue
		}
		out = append(out, line)
	}
	body := strings.TrimSpace(strings.Join(out, "\n"))
	if body == "" {
		body = "# generated by steve — isolated from the operator Codex home"
	}
	// A separate home still shares authentication. Codex defaults to loading
	// account Apps and native plugins unless these features are explicitly off.
	// Steve supplies its admitted MCP servers and skills through ACP/runtime
	// materialization; those do not require Codex's native plugin discovery.
	return []byte(body + "\n\n[features]\napps = false\nplugins = false\nremote_plugin = false\n")
}

const (
	tomlSkillsArray    = "[[skills"
	tomlSkills         = "[skills"
	tomlPlugins        = "[plugins"
	tomlMarketplaces   = "[marketplaces"
	tomlHooks          = "[hooks"
	tomlMCPServers     = "[mcp_servers"
	tomlProjects       = "[projects"
	tomlDesktop        = "[desktop"
	tomlTUI            = "[tui"
	tomlNotice         = "[notice"
	tomlFeatures       = "[features"
	tomlShellEnvPolicy = "[shell_environment_policy"
)

func dropCodexTable(header string) bool {
	prefixes := []string{
		tomlSkillsArray,
		tomlSkills,
		tomlPlugins,
		tomlMarketplaces,
		tomlHooks,
		tomlMCPServers,
		tomlProjects,
		tomlDesktop,
		tomlTUI,
		tomlNotice,
		tomlFeatures,
		tomlShellEnvPolicy,
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(header, prefix) {
			return true
		}
	}
	return false
}

func maybeKeep(out []string, line string) []string { return out }

func HasEnv(env []string, key string) bool {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

// ForPolicy fits a launch environment to the permission policy it runs
// under. Only pi needs it: pi asks for no approval, so a read-only policy is
// enforced by starting it from a home that loads no write or shell tools,
// and refused where that cannot be shown.
func ForPolicy(env []string, harnessID, policy string) ([]string, error) {
	if harnessID != harness.Pi {
		return env, nil
	}
	return pihome.Select(env, policy)
}
