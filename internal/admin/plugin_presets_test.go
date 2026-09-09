package admin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plugins"
)

func presetFixture(t *testing.T) (*PluginService, string, plugins.Manifest) {
	t.Helper()
	service, source := pluginAdminFixture(t)
	service.Admin.Cfg.Harnesses["mock"] = config.Harness{Command: "unused", Permission: "read"}
	catalog, err := agent.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	service.Admin.Catalog = catalog
	manifest := plugins.Manifest{Schema: plugins.Schema, API: plugins.API, ID: "test/admin", Version: "1.0.0", Description: "preset fixture", Skills: map[string]string{"work": "skill"}, Agents: map[string]plugins.AgentPreset{"reviewer": {Harness: "mock", Model: "base", Options: map[string]string{"effort": "medium"}, SystemPrompt: "original instructions", Skills: []string{"work"}}}}
	importPresetVersion(t, service, source, manifest, "first")
	return service, source, manifest
}

func importPresetVersion(t *testing.T, s *PluginService, source string, manifest plugins.Manifest, command string) {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewPlugin(t.Context(), plugins.Source{Kind: "directory", Location: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportPlugin(t.Context(), consoleapi.PluginImportRequest{CommandID: command, Project: "p", Digest: preview.Digest, Source: preview.Source}); err != nil {
		t.Fatal(err)
	}
	view, err := s.Plugins(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdatePlugin(t.Context(), "tools", consoleapi.PluginUpdateRequest{BaseRevision: view.Revision, Installation: plugins.Installation{PackageID: manifest.ID, Digest: preview.Digest, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"": {}}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPluginPresetUpgradePreservesOverridesAndRecoversLostResult(t *testing.T) {
	s, source, manifest := presetFixture(t)
	req := consoleapi.PluginPresetRequest{CommandID: "create", AgentID: "reviewer", Node: "", Preset: "reviewer", Digest: s.Admin.Cfg.Plugins["tools"].Digest}
	preview, err := s.PreviewPluginPreset(t.Context(), "tools", req)
	if err != nil {
		t.Fatal(err)
	}
	req.BaseRevision = preview.Revision
	result, err := s.ApplyPluginPreset(t.Context(), "tools", req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Proposed.Model != "base" || s.Admin.Cfg.Agents["reviewer"].PluginOrigin == nil {
		t.Fatal("preset was not applied")
	}
	// Simulate losing the independently stored result after configuration commit.
	if err := s.Library.Ledger.DeleteBinding(t.Context(), "plugin-preset-result", req.CommandID); err != nil {
		t.Fatal(err)
	}
	replay, err := s.ApplyPluginPreset(t.Context(), "tools", req)
	if err != nil || replay.Proposed.Model != "base" {
		t.Fatalf("commit gap replay: %+v %v", replay, err)
	}
	changed := s.Admin.Cfg.Agents["reviewer"]
	changed.Model = "user-model"
	changed.Options["effort"] = "user-effort"
	s.Admin.Cfg.Agents["reviewer"] = changed
	manifest.Version = "2.0.0"
	template := manifest.Agents["reviewer"]
	template.Model = "new-model"
	template.SystemPrompt = "new instructions"
	template.Options = map[string]string{"effort": "high", "new": "value"}
	manifest.Agents["reviewer"] = template
	importPresetVersion(t, s, source, manifest, "upgrade-package")
	req.CommandID = "upgrade-agent"
	req.Digest = s.Admin.Cfg.Plugins["tools"].Digest
	req.BaseRevision = ""
	preview, err = s.PreviewPluginPreset(t.Context(), "tools", req)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Proposed.Model != "user-model" || preview.Proposed.Options["effort"] != "user-effort" || preview.Proposed.SystemPrompt != "new instructions" || preview.Proposed.Options["new"] != "value" {
		t.Fatalf("overrides lost: %+v", preview)
	}
	req.BaseRevision = preview.Revision
	updated, err := s.ApplyPluginPreset(t.Context(), "tools", req)
	if err != nil || updated.Proposed.Model != "user-model" {
		t.Fatalf("upgrade: %+v %v", updated, err)
	}
	oldReplay, err := s.ApplyPluginPreset(t.Context(), "tools", consoleapi.PluginPresetRequest{CommandID: "create", AgentID: "other", Preset: "reviewer", Digest: req.Digest, BaseRevision: req.BaseRevision})
	if !errors.Is(err, plugins.ErrConflict) {
		t.Fatalf("command reused: %+v %v", oldReplay, err)
	}
}

func TestPluginPresetRefusesUserOwnedAgentAndStaleRevision(t *testing.T) {
	s, _, _ := presetFixture(t)
	s.Admin.Cfg.Agents["user"] = config.Agent{Harness: "mock", SystemPrompt: "user-owned"}
	req := consoleapi.PluginPresetRequest{AgentID: "user", Preset: "reviewer", Digest: s.Admin.Cfg.Plugins["tools"].Digest}
	if _, err := s.PreviewPluginPreset(t.Context(), "tools", req); err == nil {
		t.Fatal("overwrote user-owned Agent")
	}
	req.AgentID = "new"
	req.CommandID = "new"
	req.BaseRevision = "stale"
	if _, err := s.ApplyPluginPreset(t.Context(), "tools", req); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	if _, ok := s.Admin.Cfg.Agents["new"]; ok {
		t.Fatal("rejected preset created Agent")
	}
}

func TestPluginPresetReplayFinishesPendingOperation(t *testing.T) {
	s, _, _ := presetFixture(t)
	req := consoleapi.PluginPresetRequest{CommandID: "create", AgentID: "reviewer", Preset: "reviewer", Digest: s.Admin.Cfg.Plugins["tools"].Digest}
	preview, err := s.PreviewPluginPreset(t.Context(), "tools", req)
	if err != nil {
		t.Fatal(err)
	}
	req.BaseRevision = preview.Revision
	if _, err := s.ApplyPluginPreset(t.Context(), "tools", req); err != nil {
		t.Fatal(err)
	}
	operations, err := s.Library.Operations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations {
		if operation.ID == req.CommandID {
			operation.State = plugins.OperationPending
			if err := s.Library.Ledger.PutBinding(t.Context(), "plugin-management", operation.ID, operation); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.ApplyPluginPreset(t.Context(), "tools", req); err != nil {
		t.Fatal(err)
	}
	operations, err = s.Library.Operations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations {
		if operation.ID == req.CommandID && operation.State != plugins.OperationSucceeded {
			t.Fatalf("saved result left management operation pending: %+v", operation)
		}
	}
}
