package capability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
)

func TestAssemblerBuildsInstructionsAndMCP(t *testing.T) {
	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("Use the repo conventions."), 0o600); err != nil {
		t.Fatal(err)
	}
	assembler := NewAssembler(map[string]MCPServer{
		"files": {Type: "stdio", Command: "go", Args: []string{"version"}},
	})

	capabilities, err := assembler.Assemble(agent.Agent{ID: "claude", Config: agent.Config{
		SystemPrompt: "You are Claude.",
		Skills:       []string{skillDir},
		MCPServers:   []string{"files"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capabilities.Instructions, "You are Claude.") || !strings.Contains(capabilities.Instructions, "Use the repo conventions.") {
		t.Fatalf("unexpected instructions: %q", capabilities.Instructions)
	}
	if len(capabilities.MCPServers) != 1 || capabilities.MCPServers[0].Name != "files" {
		t.Fatalf("unexpected MCP servers: %#v", capabilities.MCPServers)
	}
}

func TestAssemblerRejectsUnknownMCPServer(t *testing.T) {
	_, err := NewAssembler(nil).Assemble(agent.Agent{ID: "claude", Config: agent.Config{MCPServers: []string{"missing"}}})
	if err == nil {
		t.Fatal("expected unknown MCP server error")
	}
}

func TestAssemblerRejectsInvalidMCPServer(t *testing.T) {
	tests := []struct {
		name   string
		server MCPServer
	}{
		{name: "stdio command", server: MCPServer{Type: "stdio"}},
		{name: "http URL", server: MCPServer{Type: "http", URL: "not-a-url"}},
		{name: "sse URL", server: MCPServer{Type: "sse"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assembler := NewAssembler(map[string]MCPServer{"bad": tt.server})
			_, err := assembler.Assemble(agent.Agent{Config: agent.Config{MCPServers: []string{"bad"}}})
			if err == nil {
				t.Fatal("expected invalid MCP server error")
			}
		})
	}
}
