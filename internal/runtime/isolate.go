// Package runtime prepares isolated harness homes so Steve does not inherit
// the operator's IDE skill catalog.
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
)

func CodexHome(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", harness.Codex)
}

func ClaudeHome(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", harness.ClaudeCode)
}

func GrokHome(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", harness.Grok)
}

func KimiHome(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", harness.Kimi)
}

func Prepare(stateDir string) error {
	return PrepareSelected(stateDir, []string{harness.Codex, harness.ClaudeCode, harness.Grok, harness.Kimi})
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
		case harness.Grok:
			err = PrepareGrok(GrokHome(stateDir), source(".grok"))
		case harness.Kimi:
			err = PrepareKimi(KimiHome(stateDir), source(".kimi-code"))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func SkillDests(stateDir string) []string {
	return SelectedSkillDests(stateDir, []string{harness.Codex, harness.ClaudeCode, harness.Grok, harness.Kimi})
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
		case harness.Grok:
			dest = GrokHome(stateDir)
		case harness.Kimi:
			dest = KimiHome(stateDir)
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
	case harness.Grok:
		key, value = harness.EnvGrokHome, GrokHome(stateDir)
	case harness.Kimi:
		key, value = harness.EnvKimiCodeHome, KimiHome(stateDir)
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

// FilterClaudeSettings keeps only the env block of the operator's
// settings.json: model access (base URL, custom headers, tokens) must reach
// the isolated harness, while permissions, hooks, model choice and UI
// preferences stay the operator's own.
func FilterClaudeSettings(src []byte) []byte {
	var parsed struct {
		Env map[string]json.RawMessage `json:"env"`
	}
	if err := json.Unmarshal(src, &parsed); err != nil || len(parsed.Env) == 0 {
		return []byte("{}\n")
	}
	out, err := json.MarshalIndent(map[string]any{"env": parsed.Env}, "", "  ")
	if err != nil {
		return []byte("{}\n")
	}
	return append(out, '\n')
}

func PrepareGrok(dest, userGrok string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("create grok runtime: %w", err)
	}
	var raw []byte
	if userGrok != "" {
		if err := copyAuth(dest, userGrok, "auth.json"); err != nil {
			return err
		}
		src := filepath.Join(userGrok, "config.toml")
		data, err := os.ReadFile(src)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read grok config: %w", err)
		}
		raw = data
	}
	if err := os.WriteFile(filepath.Join(dest, "config.toml"), FilterGrokConfig(raw), 0o600); err != nil {
		return fmt.Errorf("write isolated grok config: %w", err)
	}
	return os.MkdirAll(filepath.Join(dest, "skills"), 0o700)
}

// PrepareKimi prepares an isolated Kimi Code home. Credentials and OAuth
// tokens are linked, so a login in the operator's own CLI reaches the
// gateway; config.toml is copied with the operator's providers and models,
// without hooks, MCP servers, skills and terminal preferences. An operator
// without a config.toml gets none: Kimi then starts from its own defaults.
func PrepareKimi(dest, userKimi string) error {
	if err := os.MkdirAll(filepath.Join(dest, "skills"), 0o700); err != nil {
		return fmt.Errorf("create kimi runtime: %w", err)
	}
	if userKimi == "" {
		return nil
	}
	for _, name := range []string{"credentials", "oauth", "auth.json"} {
		if err := linkAuth(dest, userKimi, name); err != nil {
			return err
		}
	}
	raw, err := os.ReadFile(filepath.Join(userKimi, "config.toml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read kimi config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "config.toml"), FilterKimiConfig(raw), 0o600); err != nil {
		return fmt.Errorf("write isolated kimi config: %w", err)
	}
	return nil
}

func linkAuth(dest, userHome, name string) error {
	src := filepath.Join(userHome, name)
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	link := filepath.Join(dest, name)
	// Drop the previous link first; not-exist is the usual answer, and any
	// other failure resurfaces from Symlink as an existing path.
	_ = os.Remove(link)
	if err := os.Symlink(src, link); err != nil {
		return fmt.Errorf("link %s: %w", name, err)
	}
	return nil
}

func copyAuth(dest, userHome, name string) error {
	src := filepath.Join(userHome, name)
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", name, err)
	}
	path := filepath.Join(dest, name)
	// A link left by linkAuth would make WriteFile write into the user's
	// home, so drop it first; not-exist is the usual answer, and any other
	// failure resurfaces from WriteFile.
	_ = os.Remove(path)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

const isolatedGrokCompat = `[compat.cursor]
skills = false
rules = false
agents = false
mcps = false
hooks = false
sessions = false

[compat.claude]
skills = false
rules = false
agents = false
mcps = false
hooks = false
sessions = false

[compat.codex]
skills = false
rules = false
agents = false
mcps = false
hooks = false
sessions = false
`

func FilterGrokConfig(src []byte) []byte {
	header := "# generated by steve — isolated from the operator Grok home\n"
	if len(src) == 0 {
		return []byte(header + isolatedGrokCompat)
	}
	var out []string
	skipUntilHeader := false
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			skipUntilHeader = dropGrokTable(trimmed)
		}
		if skipUntilHeader {
			continue
		}
		out = append(out, line)
	}
	body := strings.TrimSpace(strings.Join(out, "\n"))
	if body == "" {
		return []byte(header + isolatedGrokCompat)
	}
	return []byte(body + "\n\n" + isolatedGrokCompat)
}

func dropGrokTable(header string) bool {
	prefixes := []string{
		"[skills",
		"[[skills",
		"[plugins",
		"[hooks",
		"[mcp",
		"[compat.cursor",
		"[compat.claude",
		"[compat.codex",
		"[agents",
		"[marketplaces",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(header, prefix) {
			return true
		}
	}
	return false
}

func FilterCodexConfig(src []byte) []byte {
	if len(src) == 0 {
		return []byte("# generated by steve — isolated from the operator Codex home\n")
	}
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
		return []byte("# generated by steve — isolated from the operator Codex home\n")
	}
	return []byte(body + "\n")
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

// FilterKimiConfig keeps the operator's Kimi Code model access — default
// model, providers with their OAuth storage, the model catalogue, thinking
// and loop settings — and drops what belongs to the operator's own
// terminal: hooks, MCP servers, skills, plugins, agents, cron and UI
// preferences. Skills reach the isolated home from Steve, not from here.
func FilterKimiConfig(src []byte) []byte {
	header := "# generated by steve — isolated from the operator Kimi Code home\n"
	if len(src) == 0 {
		return []byte(header)
	}
	var out []string
	skipTable := false
	skipArray := false
	topLevel := true
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if skipArray {
			// The rest of a dropped multi-line array.
			if strings.Contains(line, "]") {
				skipArray = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			topLevel = false
			skipTable = dropKimiTable(trimmed)
		}
		if skipTable || strings.HasPrefix(trimmed, "#") {
			// Comments go with what they annotate, which may be dropped;
			// the generated file carries its own header.
			continue
		}
		if topLevel && dropKimiKey(trimmed) {
			skipArray = strings.Contains(trimmed, "[") && !strings.Contains(trimmed, "]")
			continue
		}
		out = append(out, line)
	}
	body := strings.TrimSpace(strings.Join(out, "\n"))
	if body == "" {
		return []byte(header)
	}
	return []byte(header + body + "\n")
}

func dropKimiTable(header string) bool {
	prefixes := []string{
		"[hooks", "[[hooks",
		"[mcp", "[[mcp",
		"[skills", "[[skills",
		"[plugins", "[[plugins",
		"[agents", "[[agents",
		"[marketplaces", "[[marketplaces",
		"[cron", "[[cron",
		"[tui",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(header, prefix) {
			return true
		}
	}
	return false
}

// dropKimiKey names the top-level keys that are the operator's terminal
// rather than model access.
func dropKimiKey(line string) bool {
	i := strings.Index(line, "=")
	if i < 0 {
		return false
	}
	switch strings.TrimSpace(line[:i]) {
	case "extra_skill_dirs", "merge_all_available_skills", "theme", "default_editor", "show_thinking_stream":
		return true
	}
	return false
}

func HasEnv(env []string, key string) bool {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}
