package admin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/plugins"
)

func pluginAdminFixture(t *testing.T) (*PluginService, string) {
	t.Helper()
	state := t.TempDir()
	book, err := ledger.Open(filepath.Join(state, "ledger"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	disabled := false
	cfg := &config.Config{Gateway: config.Gateway{OwnerID: "owner", Level: "internal", StatePath: filepath.Join(state, "state.json")}, Projects: map[string]config.Project{"p": {Home: config.ProjectHome{Path: filepath.Join(state, "work")}}}, Agents: map[string]config.Agent{}, Harnesses: map[string]config.Harness{}, MCPServers: map[string]config.MCPServer{}, Feishu: config.Feishu{Enabled: &disabled}}
	path := filepath.Join(state, "application.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	store := &plugins.Store{Dir: filepath.Join(state, "plugins")}
	pool := &node.PluginRuntimePool{Store: store, StateDir: state}
	t.Cleanup(func() { pool.Close() })
	service := &PluginService{Admin: &Service{Cfg: cfg, Path: path}, Library: &plugins.Library{Store: store, Ledger: book}, Local: pool}
	source := t.TempDir()
	os.Mkdir(filepath.Join(source, "skill"), 0700)
	manifest := plugins.Manifest{Schema: plugins.Schema, API: plugins.API, ID: "test/admin", Version: "1.0.0", Description: "admin fixture", Skills: map[string]string{"work": "skill"}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(source, "plugin.json"), raw, 0600)
	os.WriteFile(filepath.Join(source, "skill/SKILL.md"), []byte("PLUGIN_ADMIN"), 0600)
	return service, source
}

func TestPluginManagementImportsConfiguresAndPreparesWithCAS(t *testing.T) {
	service, source := pluginAdminFixture(t)
	preview, err := service.PreviewPlugin(t.Context(), plugins.Source{Kind: "directory", Location: source})
	if err != nil {
		t.Fatal(err)
	}
	request := consoleapi.PluginImportRequest{CommandID: "import-one", Project: "p", Digest: preview.Digest, Source: preview.Source}
	record, err := service.ImportPlugin(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if record.Digest != preview.Digest {
		t.Fatal("wrong import identity")
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ImportPlugin(t.Context(), request); err != nil {
		t.Fatal("lost original source prevented replay:", err)
	}
	altered := request
	altered.Digest = strings.Repeat("a", 64)
	if _, err := service.ImportPlugin(t.Context(), altered); !errors.Is(err, plugins.ErrConflict) {
		t.Fatalf("command changed meaning: %v", err)
	}
	view, err := service.Plugins(t.Context())
	if err != nil || len(view.Packages) != 1 || len(view.Operations) != 1 || view.Operations[0].State != plugins.OperationSucceeded {
		t.Fatalf("view: %+v %v", view, err)
	}
	update := consoleapi.PluginUpdateRequest{BaseRevision: view.Revision, Installation: plugins.Installation{PackageID: preview.Manifest.ID, Digest: preview.Digest, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"": {}}}}
	configured, err := service.UpdatePlugin(t.Context(), "work", update)
	if err != nil {
		t.Fatal(err)
	}
	if len(configured.Installations) != 1 || configured.Installations[0].Installation.Enabled {
		t.Fatal("saving configuration implicitly enabled package")
	}
	if _, err := service.UpdatePlugin(t.Context(), "work", update); !errors.Is(err, consoleapi.ErrSettingsConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	ready, err := service.PreparePlugin(t.Context(), "work")
	if err != nil || len(ready.Targets) != 1 || ready.Targets[0].State != plugins.Prepared {
		t.Fatalf("prepare: %+v %v", ready, err)
	}
	update.BaseRevision = configured.Revision
	update.Installation.Enabled = true
	enabled, err := service.UpdatePlugin(t.Context(), "work", update)
	if err != nil || !enabled.Installations[0].Installation.Enabled {
		t.Fatalf("enable: %+v %v", enabled, err)
	}
	onDisk, err := config.Load(service.Admin.Path)
	if err != nil || !onDisk.Plugins["work"].Enabled {
		t.Fatalf("declaration not durable: %v", err)
	}
}

func TestPluginManagementRefusesUnknownScopeAndUnimportedContent(t *testing.T) {
	service, source := pluginAdminFixture(t)
	preview, err := service.PreviewPlugin(t.Context(), plugins.Source{Kind: "directory", Location: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ImportPlugin(t.Context(), consoleapi.PluginImportRequest{CommandID: "unknown", Project: "other", Digest: preview.Digest, Source: preview.Source}); err == nil {
		t.Fatal("import crossed unknown project")
	}
	view, err := service.Plugins(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdatePlugin(t.Context(), "not-imported", consoleapi.PluginUpdateRequest{BaseRevision: view.Revision, Installation: plugins.Installation{PackageID: preview.Manifest.ID, Digest: preview.Digest, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"": {}}}})
	if err == nil {
		t.Fatal("unimported package enabled")
	}
	if len(service.Admin.Cfg.Plugins) != 0 {
		t.Fatal("failed update changed live configuration")
	}
}
