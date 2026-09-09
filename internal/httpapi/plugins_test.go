package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plugins"
)

type pluginAPIStub struct{ updated int }

func (*pluginAPIStub) Plugins(context.Context) (consoleapi.PluginsView, error) {
	return consoleapi.PluginsView{Revision: "revision", Packages: []plugins.PackageRecord{}, Installations: []consoleapi.PluginInstallationView{}}, nil
}
func (*pluginAPIStub) PreviewPlugin(context.Context, plugins.Source) (consoleapi.PluginPreview, error) {
	return consoleapi.PluginPreview{Digest: "digest"}, nil
}
func (*pluginAPIStub) ImportPlugin(context.Context, consoleapi.PluginImportRequest) (plugins.PackageRecord, error) {
	return plugins.PackageRecord{Project: "p", Digest: "digest"}, nil
}
func (p *pluginAPIStub) UpdatePlugin(_ context.Context, _ string, req consoleapi.PluginUpdateRequest) (consoleapi.PluginsView, error) {
	p.updated++
	if req.BaseRevision != "revision" {
		return consoleapi.PluginsView{}, consoleapi.ErrSettingsConflict
	}
	return consoleapi.PluginsView{Revision: "next"}, nil
}
func (*pluginAPIStub) PreparePlugin(context.Context, string) (consoleapi.PluginInstallationView, error) {
	return consoleapi.PluginInstallationView{ID: "tools", Targets: []consoleapi.PluginTargetView{{Node: "offline", State: "unavailable", Error: "offline"}}}, nil
}
func (*pluginAPIStub) PluginSecrets(context.Context, string) ([]plugins.SecretInfo, error) {
	return []plugins.SecretInfo{{Reference: plugins.SecretRef{Name: "token", Revision: "version"}}}, nil
}

func TestPluginRoutesRequireOwnerAndPreserveRevisionConflict(t *testing.T) {
	stub := &pluginAPIStub{}
	server := &Server{token: "owner", plugins: stub}
	mux := http.NewServeMux()
	server.pluginRoutes(mux)
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	if got := call("GET", "/console/plugins", "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", got.Code)
	}
	if got := call("GET", "/console/plugins", "", "owner"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "revision") {
		t.Fatalf("list: %d %s", got.Code, got.Body.String())
	}
	if got := call("PUT", "/console/plugins/installations/tools", `{"base_revision":"old","installation":{}}`, "owner"); got.Code != http.StatusConflict {
		t.Fatalf("CAS: %d", got.Code)
	}
	before := stub.updated
	if got := call("PUT", "/console/plugins/installations/tools", `{"base_revision":"revision","unknown":true}`, "owner"); got.Code != http.StatusBadRequest || stub.updated != before {
		t.Fatal("unknown field reached application")
	}
	if got := call("PUT", "/console/plugins/installations/tools", `{"base_revision":"revision"} {}`, "owner"); got.Code != http.StatusBadRequest || stub.updated != before {
		t.Fatal("second object reached application")
	}
	if got := call("POST", "/console/plugins/installations/tools/prepare", "", "owner"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "unavailable") {
		t.Fatal("partial readiness was hidden")
	}
	secrets := call("GET", "/console/plugins/nodes/worker/secrets", "", "owner")
	var values []plugins.SecretInfo
	if err := json.Unmarshal(secrets.Body.Bytes(), &values); err != nil || len(values) != 1 {
		t.Fatalf("metadata: %s %v", secrets.Body.String(), err)
	}
	server.maintenance = true
	if got := call("POST", "/console/plugins/import", `{}`, "owner"); got.Code == http.StatusOK {
		t.Fatal("maintenance admitted import")
	}
}
