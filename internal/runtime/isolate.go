// Package runtime prepares isolated harness homes so Steve does not inherit
// the operator's IDE skill catalog.
package runtime

import (
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
	userHome, err := os.UserHomeDir()
	if err != nil {
		userHome = ""
	}
	var userCodex, userGrok, userKimi string
	if userHome != "" {
		userCodex = filepath.Join(userHome, ".codex")
		userGrok = filepath.Join(userHome, ".grok")
		userKimi = filepath.Join(userHome, ".kimi-code")
	}
	if err := PrepareCodex(CodexHome(stateDir), userCodex); err != nil {
		return err
	}
	if err := PrepareClaude(ClaudeHome(stateDir)); err != nil {
		return err
	}
	if err := PrepareGrok(GrokHome(stateDir), userGrok); err != nil {
		return err
	}
	return PrepareKimi(KimiHome(stateDir), userKimi)
}

func SkillDests(stateDir string) []string {
	return []string{
		filepath.Join(CodexHome(stateDir), "skills"),
		filepath.Join(ClaudeHome(stateDir), "skills"),
		filepath.Join(GrokHome(stateDir), "skills"),
		filepath.Join(KimiHome(stateDir), "skills"),
	}
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

func PrepareClaude(dest string) error {
	if err := os.MkdirAll(filepath.Join(dest, "skills"), 0o700); err != nil {
		return fmt.Errorf("create claude runtime: %w", err)
	}
	return nil
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

func PrepareKimi(dest, userKimi string) error {
	if err := os.MkdirAll(filepath.Join(dest, "skills"), 0o700); err != nil {
		return fmt.Errorf("create kimi runtime: %w", err)
	}
	if userKimi == "" {
		return nil
	}
	if err := linkAuth(dest, userKimi, "credentials"); err != nil {
		return err
	}
	return linkAuth(dest, userKimi, "auth.json")
}

func linkAuth(dest, userHome, name string) error {
	src := filepath.Join(userHome, name)
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	link := filepath.Join(dest, name)
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

func HasEnv(env []string, key string) bool {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}
