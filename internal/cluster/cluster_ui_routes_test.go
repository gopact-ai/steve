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
	request.Header.Set("Authorization", "Bearer "+peer.UIToken)
	response := httptest.NewRecorder()
	peer.serveUI(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "text/html") {
		t.Fatal("the desktop shell must remain available before coordination starts")
	}
}
