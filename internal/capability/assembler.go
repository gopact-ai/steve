package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
)

type MCPServer struct {
	Type    string
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Headers map[string]string
}

type Capabilities struct {
	Instructions string
	MCPServers   []acp.MCPServer
	Fingerprint  string
}

type skillFingerprinter interface {
	Fingerprint() string
}

type Assembler struct {
	servers map[string]MCPServer
	home    home.Loader
	skills  skillFingerprinter
}

func NewAssembler(servers map[string]MCPServer) *Assembler {
	return &Assembler{servers: servers}
}

func (a *Assembler) SetHome(loader home.Loader) *Assembler {
	a.home = loader
	return a
}

func (a *Assembler) SetSkills(src skillFingerprinter) *Assembler {
	a.skills = src
	return a
}

func (a *Assembler) Assemble(selected agent.Agent) (Capabilities, error) {
	return a.AssembleMode(selected, home.ModeNone)
}

func (a *Assembler) AssembleMode(selected agent.Agent, mode home.Mode) (Capabilities, error) {
	var snap home.Snapshot
	if a.home != nil && mode != home.ModeNone {
		var err error
		snap, err = a.home.Load(mode)
		if err != nil {
			return Capabilities{}, err
		}
	}
	parts := []string{}
	if snap.Identity != "" {
		parts = append(parts, snap.Identity)
	}
	if selected.SystemPrompt != "" {
		parts = append(parts, selected.SystemPrompt)
	}
	for _, root := range selected.Skills {
		data, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
		if err != nil {
			return Capabilities{}, fmt.Errorf("read skill %q: %w", root, err)
		}
		parts = append(parts, string(data))
	}
	identity := strings.Join(parts, "\n\n")
	instructions := identity
	if snap.Memory != "" {
		if instructions != "" {
			instructions += "\n\n" + snap.Memory
		} else {
			instructions = snap.Memory
		}
	}
	servers := make([]acp.MCPServer, 0, len(selected.MCPServers))
	for _, name := range selected.MCPServers {
		cfg, ok := a.servers[name]
		if !ok {
			return Capabilities{}, fmt.Errorf("unknown MCP server %q", name)
		}
		server, err := makeMCPServer(name, cfg)
		if err != nil {
			return Capabilities{}, err
		}
		servers = append(servers, server)
	}
	hashMode := home.ModeNone
	if a.home != nil {
		hashMode = mode
	}
	skillsHash := ""
	if a.skills != nil {
		skillsHash = a.skills.Fingerprint()
	}
	fp, err := fingerprint(identity, servers, hashMode, skillsHash)
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{Instructions: instructions, MCPServers: servers, Fingerprint: fp}, nil
}

func makeMCPServer(name string, cfg MCPServer) (acp.MCPServer, error) {
	switch cfg.Type {
	case "stdio":
		if strings.TrimSpace(cfg.Command) == "" {
			return acp.MCPServer{}, fmt.Errorf("MCP server %q command is required", name)
		}
		if _, err := exec.LookPath(cfg.Command); err != nil {
			return acp.MCPServer{}, fmt.Errorf("MCP server %q command %q: %w", name, cfg.Command, err)
		}
		env := make([]acp.EnvVariable, 0, len(cfg.Env))
		for _, key := range sortedKeys(cfg.Env) {
			value := cfg.Env[key]
			env = append(env, acp.EnvVariable{Name: key, Value: value})
		}
		return acp.StdioMCPServer(name, cfg.Command, cfg.Args, env), nil
	case "http", "sse":
		parsed, err := url.ParseRequestURI(cfg.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return acp.MCPServer{}, fmt.Errorf("MCP server %q has invalid URL %q", name, cfg.URL)
		}
		headers := make([]acp.HTTPHeader, 0, len(cfg.Headers))
		for _, key := range sortedKeys(cfg.Headers) {
			value := cfg.Headers[key]
			headers = append(headers, acp.HTTPHeader{Name: key, Value: value})
		}
		if cfg.Type == "http" {
			return acp.HTTPMCPServer(name, cfg.URL, headers), nil
		}
		return acp.SSEMCPServer(name, cfg.URL, headers), nil
	default:
		return acp.MCPServer{}, fmt.Errorf("MCP server %q has unknown type %q", name, cfg.Type)
	}
}

func fingerprint(instructions string, servers []acp.MCPServer, mode home.Mode, skillsHash string) (string, error) {
	data, err := json.Marshal(struct {
		Instructions string          `json:"instructions"`
		MCPServers   []acp.MCPServer `json:"mcp_servers"`
		HomeMode     string          `json:"home_mode,omitempty"`
		Skills       string          `json:"skills,omitempty"`
	}{instructions, servers, string(mode), skillsHash})
	if err != nil {
		return "", fmt.Errorf("fingerprint capabilities: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
