// Package plugins reads and prepares immutable capability packages. Preparing
// a package never activates it or starts its tools.
package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	Schema           = 1
	API              = "steve.plugins.v1"
	ManifestFile     = "plugin.json"
	MaxManifestBytes = 128 << 10
	MaxPackageBytes  = 32 << 20
	MaxFileBytes     = 8 << 20
	MaxFiles         = 4096
)

var (
	ErrInvalid      = errors.New("invalid plugin package")
	ErrIncompatible = errors.New("incompatible plugin package")
	ErrConflict     = errors.New("plugin identity conflict")
	ErrIntegrity    = errors.New("plugin content integrity mismatch")
	nameShape       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	versionShape    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[a-z0-9][a-z0-9.-]*)?$`)
	digestShape     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Manifest struct {
	Schema      int                    `json:"schema"`
	API         string                 `json:"api"`
	ID          string                 `json:"id"`
	Version     string                 `json:"version"`
	Description string                 `json:"description"`
	Platforms   []Platform             `json:"platforms,omitempty"`
	Settings    map[string]Setting     `json:"settings,omitempty"`
	Skills      map[string]string      `json:"skills,omitempty"`
	MCP         map[string]MCPServer   `json:"mcp,omitempty"`
	Agents      map[string]AgentPreset `json:"agents,omitempty"`
}

type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type Setting struct {
	Description string  `json:"description"`
	Secret      bool    `json:"secret,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Default     *string `json:"default,omitempty"`
}

// Value references a declared setting or node-local secret. Prefix is useful
// for authorization headers without storing a credential in the package.
type Value struct {
	Text   string `json:"text,omitempty"`
	Config string `json:"config,omitempty"`
	Secret string `json:"secret,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

type MCPServer struct {
	Transport string           `json:"transport"`
	URL       Value            `json:"url,omitempty"`
	Headers   map[string]Value `json:"headers,omitempty"`
	Program   *Program         `json:"program,omitempty"`
	Env       map[string]Value `json:"env,omitempty"`
}

// Program names code inside the immutable package. Runtime, when present,
// names an existing interpreter on the selected node; preparation does not
// install or execute it. Runtime observations belong to later node admission.
type Program struct {
	Path    string  `json:"path"`
	Runtime string  `json:"runtime,omitempty"`
	Args    []Value `json:"args,omitempty"`
}

type AgentPreset struct {
	Harness      string            `json:"harness"`
	Model        string            `json:"model,omitempty"`
	Options      map[string]string `json:"options,omitempty"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	Skills       []string          `json:"skills,omitempty"`
	MCPServers   []string          `json:"mcp_servers,omitempty"`
}

func ValidID(id string) bool {
	parts := strings.Split(id, "/")
	return len(parts) == 2 && nameShape.MatchString(parts[0]) && nameShape.MatchString(parts[1])
}

func (m Manifest) Validate() error {
	if m.Schema != Schema || m.API != API {
		return fmt.Errorf("%w: schema %d, api %q", ErrIncompatible, m.Schema, m.API)
	}
	if !ValidID(m.ID) || !validVersion(m.Version) || strings.TrimSpace(m.Description) == "" {
		return fmt.Errorf("%w: id, exact version and description are required", ErrInvalid)
	}
	if len(m.Skills)+len(m.MCP)+len(m.Agents) == 0 {
		return fmt.Errorf("%w: no capabilities", ErrInvalid)
	}
	if err := m.validatePlatforms(); err != nil {
		return err
	}
	for _, key := range sortedKeys(m.Settings) {
		setting := m.Settings[key]
		if !nameShape.MatchString(key) || strings.TrimSpace(setting.Description) == "" || setting.Secret && setting.Default != nil {
			return fmt.Errorf("%w: setting %q needs a name and description; secrets cannot have defaults", ErrInvalid, key)
		}
	}
	for _, name := range sortedKeys(m.Skills) {
		if !nameShape.MatchString(name) || !packagePath(m.Skills[name]) {
			return fmt.Errorf("%w: skill %q has an invalid name or path", ErrInvalid, name)
		}
	}
	for _, name := range sortedKeys(m.MCP) {
		if !nameShape.MatchString(name) {
			return fmt.Errorf("%w: MCP name %q", ErrInvalid, name)
		}
		if err := m.validateMCP(m.MCP[name]); err != nil {
			return fmt.Errorf("MCP %s: %w", name, err)
		}
	}
	return m.validateAgents()
}

func (m Manifest) validatePlatforms() error {
	seen := map[Platform]bool{}
	for _, p := range m.Platforms {
		if !slices.Contains([]string{"linux", "darwin"}, p.OS) || !slices.Contains([]string{"amd64", "arm64"}, p.Arch) || seen[p] {
			return fmt.Errorf("%w: unsupported or repeated platform %s/%s", ErrInvalid, p.OS, p.Arch)
		}
		seen[p] = true
	}
	return nil
}

func (m Manifest) validateAgents() error {
	for _, name := range sortedKeys(m.Agents) {
		preset := m.Agents[name]
		if !nameShape.MatchString(name) || !nameShape.MatchString(preset.Harness) {
			return fmt.Errorf("%w: agent %q needs a name and existing harness", ErrInvalid, name)
		}
		for _, skill := range preset.Skills {
			if _, ok := m.Skills[skill]; !ok {
				return fmt.Errorf("%w: agent %s references unknown skill %s", ErrInvalid, name, skill)
			}
		}
		for _, server := range preset.MCPServers {
			if _, ok := m.MCP[server]; !ok {
				return fmt.Errorf("%w: agent %s references unknown MCP %s", ErrInvalid, name, server)
			}
		}
		if duplicate(preset.Skills) || duplicate(preset.MCPServers) {
			return fmt.Errorf("%w: agent %s repeats a capability", ErrInvalid, name)
		}
	}
	return nil
}

func sortedKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func duplicate(items []string) bool {
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item] {
			return true
		}
		seen[item] = true
	}
	return false
}

// CapabilityID is the full logical identity. Names exposed through a native
// harness are derived from it, while receipts retain the full identity.
func CapabilityID(id, kind, name string) (string, error) {
	if !ValidID(id) || !slices.Contains([]string{"skill", "mcp", "agent"}, kind) || !nameShape.MatchString(name) {
		return "", fmt.Errorf("%w: capability identity", ErrInvalid)
	}
	return id + "/" + kind + "/" + name, nil
}

func NativeName(id, kind, name string) (string, error) {
	identity, err := CapabilityID(id, kind, name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(identity))
	return "sp_" + hex.EncodeToString(sum[:])[:48], nil
}

func validVersion(version string) bool {
	if len(version) > 80 || !versionShape.MatchString(version) {
		return false
	}
	_, suffix, prerelease := strings.Cut(version, "-")
	if !prerelease {
		return true
	}
	for _, part := range strings.Split(suffix, ".") {
		if part == "" {
			return false
		}
		numeric := true
		for _, c := range part {
			if c < '0' || c > '9' {
				numeric = false
			}
		}
		if numeric && len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}
