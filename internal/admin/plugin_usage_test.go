package admin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/state"
)

func installedRuntime(t *testing.T) (*PluginService, plugins.RuntimeRef) {
	t.Helper()
	service, source := pluginAdminFixture(t)
	preview, err := service.PreviewPlugin(t.Context(), plugins.Source{Kind: "directory", Location: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ImportPlugin(t.Context(), consoleapi.PluginImportRequest{CommandID: "import", Project: "p", Digest: preview.Digest, Source: preview.Source}); err != nil {
		t.Fatal(err)
	}
	view, err := service.Plugins(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdatePlugin(t.Context(), "tools", consoleapi.PluginUpdateRequest{BaseRevision: view.Revision, Installation: plugins.Installation{PackageID: preview.Manifest.ID, Digest: preview.Digest, Projects: []string{"p"}, Targets: map[string]plugins.Configuration{"": {}}}}); err != nil {
		t.Fatal(err)
	}
	deployment, err := service.PreparePlugin(t.Context(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	selection := plugins.Selection{Project: "p", Harness: "mock", Deployments: []string{deployment.Targets[0].Receipt.Hash}}
	runtime, err := service.Local.Prepare(t.Context(), "unreferenced", selection, harness.Config{Command: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	return service, runtime.Ref
}

func TestRemovalPreservesArchivedSessionsAndUnconfirmedProcesses(t *testing.T) {
	service, ref := installedRuntime(t)
	sessions, err := state.OpenLedger(service.Library.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SaveSession(state.Session{ConversationID: "chat", AgentID: "agent", HarnessID: "mock", ProjectID: "p", UpstreamID: "ps_" + ref.ID + ":native", PluginRuntime: &ref}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.ArchiveSession("chat", "agent", "2026-09-09T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	usage, err := service.PluginUsage(t.Context(), "tools")
	if err != nil || len(usage.References) != 1 || usage.References[0].Kind != "archive" {
		t.Fatalf("archive reference lost: %+v %v", usage, err)
	}
	view, _ := service.Plugins(t.Context())
	if _, err := service.RemovePlugin(t.Context(), "tools", consoleapi.PluginRemoveRequest{BaseRevision: view.Revision}); !errors.Is(err, plugins.ErrRuntimeBusy) {
		t.Fatalf("removed archive: %v", err)
	}
	if err := sessions.ForgetPluginRuntime(ref.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Local.Store.BeginRuntimeUse(t.Context(), ref, "unknown-native", "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RemovePlugin(t.Context(), "tools", consoleapi.PluginRemoveRequest{BaseRevision: view.Revision}); !errors.Is(err, plugins.ErrRuntimeBusy) {
		t.Fatalf("missing stop proof allowed removal: %v", err)
	}
	if err := service.Local.Store.EndRuntimeUse(t.Context(), ref, "unknown-native"); err != nil {
		t.Fatal(err)
	}
	removed, err := service.RemovePlugin(t.Context(), "tools", consoleapi.PluginRemoveRequest{BaseRevision: view.Revision})
	if err != nil || len(removed.Installations) != 0 {
		t.Fatalf("safe removal: %+v %v", removed, err)
	}
	if _, err := service.Library.Get(context.Background(), "p", ref.Selection.Deployments[0]); err == nil {
		t.Fatal("deployment digest was treated as package digest")
	}
	if len(removed.Packages) != 1 {
		t.Fatal("removal deleted project package history")
	}
}

func TestRemovalRejectsOfflineUnverifiableTarget(t *testing.T) {
	service, ref := installedRuntime(t)
	// Retained references to a machine stay blockers even if current config no
	// longer targets that machine; a lost connection is not stop evidence.
	sessions, err := state.OpenLedger(service.Library.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	other := *ref.Clone()
	other.Selection.Node = "offline"
	other.ID = strings.Repeat("b", 64)
	if err := sessions.SaveSession(state.Session{ConversationID: "chat", AgentID: "agent", PluginRuntime: &other}); err != nil {
		t.Fatal(err)
	}
	view, _ := service.Plugins(t.Context())
	if _, err := service.RemovePlugin(t.Context(), "tools", consoleapi.PluginRemoveRequest{BaseRevision: view.Revision}); err == nil {
		t.Fatal("removed an unverifiable reference")
	}
}

func TestRevokedTargetRemainsInRemovalInventoryWithoutASessionReceipt(t *testing.T) {
	s, _ := installedRuntime(t)
	old := s.Admin.Cfg.Plugins["tools"]
	old.Targets["retired-worker"] = plugins.Configuration{}
	if err := s.Library.RememberTargets(t.Context(), "tools", old); err != nil {
		t.Fatal(err)
	}
	delete(s.Admin.Cfg.Plugins["tools"].Targets, "retired-worker")
	usage, err := s.PluginUsage(t.Context(), "tools")
	if err != nil || usage.Errors["retired-worker"] == "" {
		t.Fatalf("lost worker disappeared with no returned session receipt: %+v %v", usage, err)
	}
	view, err := s.Plugins(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemovePlugin(t.Context(), "tools", consoleapi.PluginRemoveRequest{BaseRevision: view.Revision}); !errors.Is(err, plugins.ErrRuntimeBusy) {
		t.Fatalf("offline historical worker no longer blocked removal: %v", err)
	}
}
