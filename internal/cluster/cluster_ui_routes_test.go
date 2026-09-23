package cluster

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDesktopApplicationReadsDoNotFallBackToHTML(t *testing.T) {
	peer := &Peer{UIToken: "isolated-ui-token"}
	for _, path := range []string{"/state", "/usage", "/events", "/history", "/console/tasks", "/bootstrap/node", "/dist/steve-node"} {
		t.Run(path, func(t *testing.T) {
			for _, authorized := range []bool{false, true} {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.Host = "127.0.0.1:7710"
				want := http.StatusUnauthorized
				if authorized {
					request.Header.Set("Authorization", "Bearer "+peer.UIToken)
					want = http.StatusServiceUnavailable
				}
				response := httptest.NewRecorder()
				peer.serveUI(response, request)
				if response.Code != want || strings.Contains(response.Header().Get("Content-Type"), "text/html") {
					t.Fatalf("authorized=%v: status=%d type=%q; want %d, never a page", authorized, response.Code, response.Header().Get("Content-Type"), want)
				}
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "127.0.0.1:7710"
	request.Header.Set("Authorization", "Bearer "+peer.UIToken)
	response := httptest.NewRecorder()
	peer.serveUI(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "text/html") {
		t.Fatal("the desktop shell must remain available before coordination starts")
	}
}

// The UI listener is loopback-only; pages on other sites reach it through the
// owner's browser and must be refused before the token or proxy sees them.
func TestDesktopApplicationRefusesOtherSites(t *testing.T) {
	peer := &Peer{UIToken: "isolated-ui-token"}
	rebound := httptest.NewRequest(http.MethodGet, "/", nil)
	rebound.Host = "evil.example:7710"
	rebound.Header.Set("Authorization", "Bearer "+peer.UIToken)
	crossSite := httptest.NewRequest(http.MethodPost, "/console/send", strings.NewReader("{}"))
	crossSite.Host = "127.0.0.1:7710"
	crossSite.Header.Set("Origin", "https://evil.example")
	crossSite.Header.Set("Authorization", "Bearer "+peer.UIToken)
	for _, request := range []*http.Request{rebound, crossSite} {
		response := httptest.NewRecorder()
		peer.serveUI(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s from %q: status=%d, want 403", request.Method, request.Host, request.Header.Get("Origin"), response.Code)
		}
	}
}
