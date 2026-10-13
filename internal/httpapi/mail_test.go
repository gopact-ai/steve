package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/sameorigin"
)

type mailControlStub struct {
	calls int
	body  string
}

func (m *mailControlStub) Exchange(_ context.Context, method, path string, body json.RawMessage) (json.RawMessage, int, error) {
	m.calls++
	m.body = string(body)
	return json.RawMessage(`{"items":[]}`), 200, nil
}

func TestMailboxAdministrationRequiresOwnerAndDoesNotUseConnectionOrEventAPIs(t *testing.T) {
	control := &mailControlStub{}
	server := &Server{token: "owner", mail: control}
	mux := http.NewServeMux()
	server.mailRoutes(mux)
	handler := sameorigin.Guard(mux, sameorigin.Network)
	call := func(method, path, body, token, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, token := range []string{"", "connection-token"} {
		if w := call("POST", "/console/mail/imports/preview", `{"data":"private"}`, token, ""); w.Code != 401 {
			t.Fatalf("unowned request: %d", w.Code)
		}
	}
	if control.calls != 0 {
		t.Fatal("unauthorized request reached the credential runtime")
	}
	if w := call("POST", "/console/mail/imports/preview", `{"data":"private"}`, "owner", "https://evil.test"); w.Code != 403 {
		t.Fatalf("cross-origin: %d", w.Code)
	}
	if w := call("POST", "/console/mail/imports/preview", `{"data":"private"}`, "owner", ""); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("owner: %d", w.Code)
	}
	if control.calls != 1 || control.body != `{"data":"private"}` {
		t.Fatal("mail import did not stay on its private administration port")
	}
	if w := call("POST", "/console/mail/imports/preview", "invalid", "owner", ""); w.Code != 400 {
		t.Fatalf("malformed: %d", w.Code)
	}
	server.mail = nil
	if w := call("GET", "/console/mail/accounts", "", "owner", ""); w.Code != 503 || !strings.Contains(w.Body.String(), "mail_unavailable") {
		t.Fatalf("disconnected: %d %s", w.Code, w.Body.String())
	}
}
