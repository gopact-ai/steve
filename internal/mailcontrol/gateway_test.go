package mailcontrol

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayPinsLocalOriginAndNeverForwardsOwnerCredentials(t *testing.T) {
	const token = "provider-administration-token-32-characters"
	var got string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Origin") != "" {
			t.Error("unexpected credentials or browser origin")
		}
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer provider.Close()
	g := New(provider.URL, token)
	raw, status, err := g.Exchange(context.Background(), "POST", "/imports/preview", []byte(`{"data":"fixture"}`))
	if err != nil || status != 200 || string(raw) != `{"items":[]}` || got != `{"data":"fixture"}` {
		t.Fatalf("exchange: %d %s %v", status, raw, err)
	}
	for _, path := range []string{"//evil.test", "/../exports", "/accounts/x?next=http://evil.test", "/accounts/x%2fsync"} {
		if _, _, err := g.Exchange(context.Background(), "GET", path, nil); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	for _, address := range []string{"https://127.0.0.1:8787", "http://localhost:8787", "http://127.0.0.1:8787/path", "http://127.0.0.1:8787?token=hidden", "http://evil.test:8787"} {
		if New(address, token).setup == nil {
			t.Errorf("accepted non-pinned origin %q", address)
		}
	}
}

func TestGatewayRefusesRedirectsAndSuppressesTransportDetails(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true; _, _ = w.Write([]byte(`{}`)) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	_, status, err := New(redirect.URL, strings.Repeat("s", 32)).Exchange(context.Background(), "GET", "/accounts", nil)
	if err == nil || leaked || status != 503 || strings.Contains(err.Error(), redirect.URL) {
		t.Fatalf("redirect: %d %v leaked=%v", status, err, leaked)
	}
}
