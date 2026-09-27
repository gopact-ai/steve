package app

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

// A cluster node answers a request that names no language in the language
// the Hub is set to, and follows the setting when the owner changes it. In
// a cluster the setting is kept in the shared configuration; the node's own
// file still names the language it was installed with.
func TestAClusterNodeSpeaksTheLanguageTheHubIsSetTo(t *testing.T) {
	dir := ClusterPeerTestDir(t)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	options, _ := testPeerOptions(t, filepath.Join(dir, "app"), nil)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	for _, locale := range []string{"en", "zh"} {
		status, body := PeerRequest(t, peer, http.MethodGet, "/console/settings", nil)
		var settings consoleapi.SettingsView
		if status != http.StatusOK || json.Unmarshal(body, &settings) != nil {
			t.Fatalf("read settings: %d %s", status, body)
		}
		status, body = PeerRequest(t, peer, http.MethodPut, "/console/settings", consoleapi.SettingsUpdate{BaseRevision: settings.Revision, Settings: json.RawMessage(`{"gateway":{"locale":"` + locale + `"}}`)})
		if status != http.StatusOK {
			t.Fatalf("set the Hub to %s: %d %s", locale, status, body)
		}
		// The coordination page is this node's own answer, not the
		// application's; its reason is said in the Hub's language.
		status, body = PeerRequest(t, peer, http.MethodGet, "/console/coordination", nil)
		var view consoleapi.CoordinationView
		if status != http.StatusOK || json.Unmarshal(body, &view) != nil || view.Reason == "" {
			t.Fatalf("coordination: %d %s", status, body)
		}
		if chinese := strings.IndexFunc(view.Reason, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0; chinese != (locale == "zh") {
			t.Errorf("with the Hub set to %s the node says %q", locale, view.Reason)
		}
	}
}
