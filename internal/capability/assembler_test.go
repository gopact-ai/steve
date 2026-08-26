package capability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
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

func TestAssembleWithoutLoaderMatchesFingerprint(t *testing.T) {
	agentCfg := agent.Agent{ID: "codex", Config: agent.Config{SystemPrompt: "base"}}
	first, err := NewAssembler(nil).Assemble(agentCfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAssembler(nil).AssembleMode(agentCfg, home.ModeNone)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint || first.Instructions != "base" {
		t.Fatalf("nil-loader fingerprint drifted: %#v %#v", first, second)
	}
}

func TestAssembleModeHashesIdentityNotMemory(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileMemory), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	assembler := NewAssembler(nil).SetHome(home.Dir{Path: dir})
	first, err := assembler.AssembleMode(agent.Agent{ID: "codex"}, home.ModeOwner)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Instructions, "alpha") || !strings.Contains(first.Instructions, "Steve home") {
		t.Fatalf("owner instructions: %s", first.Instructions)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileMemory), []byte("beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := assembler.AssembleMode(agent.Agent{ID: "codex"}, home.ModeOwner)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatal("MEMORY edit changed fingerprint")
	}
	if !strings.Contains(second.Instructions, "beta") {
		t.Fatal("MEMORY edit not injected")
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileSoul), []byte("new soul"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := assembler.AssembleMode(agent.Agent{ID: "codex"}, home.ModeOwner)
	if err != nil {
		t.Fatal(err)
	}
	if third.Fingerprint == first.Fingerprint {
		t.Fatal("SOUL edit did not change fingerprint")
	}
	guest, err := assembler.AssembleMode(agent.Agent{ID: "codex"}, home.ModeGuest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(guest.Instructions, "beta") || strings.Contains(guest.Instructions, dir) {
		t.Fatalf("guest leaked memory or path: %s", guest.Instructions)
	}
}

func TestAssembleHashesSkillMap(t *testing.T) {
	src := &fakeSkills{hash: "aaa"}
	assembler := NewAssembler(nil).SetSkills(src)
	first, err := assembler.Assemble(agent.Agent{ID: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	src.hash = "bbb"
	second, err := assembler.Assemble(agent.Agent{ID: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("enabling a skill did not change fingerprint")
	}
}

type fakeSkills struct{ hash string }

func (s *fakeSkills) Fingerprint() string { return s.hash }

func TestAssemblerNilSkillsMatchesLegacyFingerprint(t *testing.T) {
	with := NewAssembler(nil).SetSkills(&fakeSkills{})
	without := NewAssembler(nil)
	agentCfg := agent.Agent{ID: "codex", Config: agent.Config{SystemPrompt: "base"}}
	first, err := without.Assemble(agentCfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := with.Assemble(agentCfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatal("empty skills hash should omit from fingerprint")
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
