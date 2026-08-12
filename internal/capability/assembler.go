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

type Assembler struct{ servers map[string]MCPServer }

func NewAssembler(servers map[string]MCPServer) *Assembler {
	return &Assembler{servers: servers}
}

func (a *Assembler) Assemble(selected agent.Agent) (Capabilities, error) {
	parts := []string{}
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
	instructions := strings.Join(parts, "\n\n")
	fingerprint, err := fingerprint(instructions, servers)
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{Instructions: instructions, MCPServers: servers, Fingerprint: fingerprint}, nil
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

func fingerprint(instructions string, servers []acp.MCPServer) (string, error) {
	data, err := json.Marshal(struct {
		Instructions string          `json:"instructions"`
		MCPServers   []acp.MCPServer `json:"mcp_servers"`
	}{instructions, servers})
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
