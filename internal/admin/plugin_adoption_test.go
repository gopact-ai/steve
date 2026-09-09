package admin

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plugins"
)

func TestPluginAdoptionMovesOnlyReviewedAgentAttachments(t *testing.T) {
	s, source, manifest := presetFixture(t)
	template := manifest.Agents["reviewer"]
	template.MCPServers = []string{"api"}
	manifest.Agents["reviewer"] = template
	manifest.MCP = map[string]plugins.MCPServer{"api": {Transport: "http", URL: plugins.Value{Text: "https://example.invalid"}}}
	manifest.Version = "1.1.0"
	importPresetVersion(t, s, source, manifest, "with-mcp")
	s.Admin.Cfg.MCPServers["old-api"] = config.MCPServer{Type: "http", URL: "https://legacy.invalid"}
	s.Admin.Cfg.Agents["user"] = config.Agent{Harness: "mock", Model: "user-model", SystemPrompt: "user instructions", Skills: []string{"/legacy/work", "/legacy/keep"}, MCPServers: []string{"old-api"}}
	s.Admin.Cfg.Agents["other"] = s.Admin.Cfg.Agents["user"]
	primary := s.Admin.Cfg.Agents["user"]
	primary.Default = true
	s.Admin.Cfg.Agents["user"] = primary
	req := consoleapi.PluginPresetRequest{CommandID: "adopt", AgentID: "user", Preset: "reviewer", Digest: s.Admin.Cfg.Plugins["tools"].Digest, Adopt: &plugins.Adoption{Skills: map[string]string{"/legacy/work": "work"}, MCP: map[string]string{"old-api": "api"}}}
	preview, err := s.PreviewPluginPreset(t.Context(), "tools", req)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Existing.Skills) != 2 || len(preview.Proposed.Skills) != 1 || len(preview.Proposed.MCPServers) != 0 || preview.Proposed.Model != "user-model" || preview.Proposed.SystemPrompt != "user instructions" {
		t.Fatalf("migration preview lost user settings: %+v", preview)
	}
	req.BaseRevision = preview.Revision
	if _, err := s.ApplyPluginPreset(t.Context(), "tools", req); err != nil {
		t.Fatal(err)
	}
	if len(s.Admin.Cfg.Agents["other"].Skills) != 2 || len(s.Admin.Cfg.MCPServers) != 1 {
		t.Fatal("adoption changed reusable user-owned sources")
	}
	adopted := s.Admin.Cfg.Agents["user"]
	if len(adopted.Skills) != 1 || len(adopted.MCPServers) != 0 || adopted.PluginOrigin.Adopted.Skills["/legacy/work"] != "work" {
		t.Fatal("legacy attachments were duplicated or origin lost")
	}
	// A response lost after the config commit reuses its immutable result.
	if err := s.Library.Ledger.DeleteBinding(t.Context(), "plugin-preset-result", req.CommandID); err != nil {
		t.Fatal(err)
	}
	replay, err := s.ApplyPluginPreset(t.Context(), "tools", req)
	if err != nil || len(replay.Proposed.Skills) != 1 || len(replay.Proposed.MCPServers) != 0 {
		t.Fatalf("adoption replay lost attachment result: %+v %v", replay, err)
	}
	adopted.MCPServers = []string{"old-api"}
	s.Admin.Cfg.Agents["user"] = adopted
	if _, err := s.Admin.Cfg.AgentCatalog(); !errors.Is(err, plugins.ErrConflict) {
		t.Fatalf("old editor restored duplicate attachment: %v", err)
	}
}

func TestPluginAdoptionRejectsUnreviewedSourceAndUnrelatedOrigin(t *testing.T) {
	s, _, _ := presetFixture(t)
	s.Admin.Cfg.Agents["user"] = config.Agent{Harness: "mock", Skills: []string{"/legacy/work"}}
	req := consoleapi.PluginPresetRequest{AgentID: "user", Preset: "reviewer", Digest: s.Admin.Cfg.Plugins["tools"].Digest, Adopt: &plugins.Adoption{Skills: map[string]string{"/not/attached": "work"}}}
	if _, err := s.PreviewPluginPreset(t.Context(), "tools", req); !errors.Is(err, plugins.ErrInvalid) {
		t.Fatalf("unattached source adopted: %v", err)
	}
	req.Adopt.Skills = map[string]string{"/legacy/work": "missing"}
	if _, err := s.PreviewPluginPreset(t.Context(), "tools", req); !errors.Is(err, plugins.ErrInvalid) {
		t.Fatalf("unknown capability adopted: %v", err)
	}
}
