package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func checkPeerPluginTurn(t *testing.T, peer *cluster.Peer, conversation, command, version string) {
	t.Helper()
	var result struct {
		Reply consoleapi.Reply `json:"reply"`
	}
	pluginPeerJSON(t, peer, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "plugincheck manual review", CommandID: command}, &result)
	if result.Reply.Error != "" || !strings.Contains(result.Reply.Text, "PLUGIN_SKILL_github_"+version) || !strings.Contains(result.Reply.Text, "REVIEW_EVIDENCE/team-tools/"+version) {
		t.Fatalf("manual review used wrong package version: %+v", result.Reply)
	}
}

func checkPeerPluginProjectIsolation(t *testing.T, coordinator, worker *cluster.Peer) {
	t.Helper()
	path := filepath.Join(worker.Config.DataDir, "unrelated-plugin-project")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	pluginPeerJSON(t, coordinator, http.MethodPost, "/console/projects", consoleapi.AddProjectRequest{ID: "unrelated", Node: worker.Config.NodeID, Path: path, Repo: "inplace", Level: "internal"}, nil)
	conversation := "console:plugin-project-isolation"
	pluginPeerJSON(t, coordinator, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "/project use unrelated", CommandID: "isolation-project"}, nil)
	var result struct {
		Reply consoleapi.Reply `json:"reply"`
	}
	pluginPeerJSON(t, coordinator, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "plugincheck manual review", CommandID: "isolation-review"}, &result)
	if result.Reply.Error != "" || !strings.Contains(result.Reply.Text, "[plugins: ]") || strings.Contains(result.Reply.Text, "PLUGIN_SKILL_") || strings.Contains(result.Reply.Text, "REVIEW_EVIDENCE") {
		t.Fatalf("plugin capabilities crossed project scope: %+v", result.Reply)
	}
}

func checkPeerPluginRollbackAndRemoval(t *testing.T, peer *cluster.Peer, conversation string) {
	t.Helper()
	checkPeerPluginTurn(t, peer, conversation, "review-v2", "2.0.0")
	var view consoleapi.PluginsView
	pluginPeerJSON(t, peer, http.MethodGet, "/console/plugins", nil, &view)
	for _, item := range append([]consoleapi.PluginInstallationView(nil), view.Installations...) {
		for _, record := range view.Packages {
			if record.Manifest.ID == item.Installation.PackageID && record.Manifest.Version == "1.0.0" {
				item.Installation.Digest = record.Digest
			}
		}
		pluginPeerJSON(t, peer, http.MethodPut, "/console/plugins/installations/"+item.ID, consoleapi.PluginUpdateRequest{BaseRevision: view.Revision, Installation: item.Installation}, &view)
	}
	checkPeerPluginTurn(t, peer, conversation, "retained-v2-after-rollback", "2.0.0")
	other := "console:plugin-rollback"
	pluginPeerJSON(t, peer, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: other, Input: "/project use workspace", CommandID: "rollback-project"}, nil)
	checkPeerPluginTurn(t, peer, other, "review-v1-after-rollback", "1.0.0")
	for _, item := range append([]consoleapi.PluginInstallationView(nil), view.Installations...) {
		item.Installation.Enabled = false
		pluginPeerJSON(t, peer, http.MethodPut, "/console/plugins/installations/"+item.ID, consoleapi.PluginUpdateRequest{BaseRevision: view.Revision, Installation: item.Installation}, &view)
	}
	status, _ := PeerRequest(t, peer, http.MethodDelete, "/console/plugins/installations/github", consoleapi.PluginRemoveRequest{BaseRevision: view.Revision})
	if status == http.StatusOK {
		t.Fatal("removed packages still bound to idle sessions")
	}
	var usage consoleapi.PluginUsageView
	pluginPeerJSON(t, peer, http.MethodGet, "/console/plugins/installations/github/usage", nil, &usage)
	if len(usage.References) == 0 || len(usage.Runtimes) < 2 {
		t.Fatalf("old versions disappeared from inventory: %+v", usage)
	}
	for _, runtime := range usage.Runtimes {
		pluginPeerJSON(t, peer, http.MethodPost, "/console/plugins/installations/github/runtimes/"+runtime.Ref.ID+"/close", struct{}{}, nil)
	}
	for _, id := range []string{"github", "team-tools"} {
		pluginPeerJSON(t, peer, http.MethodDelete, "/console/plugins/installations/"+id, consoleapi.PluginRemoveRequest{BaseRevision: view.Revision}, &view)
	}
	view = consoleapi.PluginsView{}
	pluginPeerJSON(t, peer, http.MethodGet, "/console/plugins", nil, &view)
	if len(view.Installations) != 0 || len(view.Packages) != 4 || len(view.Agents) != 1 || view.Agents[0].Origin != nil {
		t.Fatalf("removal lost package history or user Agent: %+v", view)
	}
}
