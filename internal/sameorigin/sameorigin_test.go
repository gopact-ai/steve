package sameorigin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func request(method, host, origin string) *http.Request {
	r := httptest.NewRequest(method, "/console/send", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestLoopbackSurfaceAcceptsOnlyLoopbackHosts(t *testing.T) {
	for _, host := range []string{"127.0.0.1:7710", "[::1]:7710", "localhost:7710", "LOCALHOST", "console.localhost:7710", "127.0.0.2"} {
		if err := Check(request(http.MethodGet, host, ""), true); err != nil {
			t.Errorf("host %q refused: %v", host, err)
		}
	}
	// A rebound name resolves to loopback but still names the attacker's site.
	for _, host := range []string{"evil.example:7710", "evil.example", "", "10.0.0.5:7710", "localhost.evil.example"} {
		if err := Check(request(http.MethodGet, host, ""), true); err == nil {
			t.Errorf("host %q accepted on a loopback surface", host)
		}
	}
	if err := Check(request(http.MethodGet, "hub.internal:7710", ""), false); err != nil {
		t.Errorf("a network surface names itself however it is reached: %v", err)
	}
}

func TestWritesRequireASameOriginCaller(t *testing.T) {
	accepted := []*http.Request{
		request(http.MethodPost, "127.0.0.1:7710", ""),
		request(http.MethodPost, "127.0.0.1:7710", "http://127.0.0.1:7710"),
		request(http.MethodPut, "localhost:7710", "http://LOCALHOST:7710"),
		request(http.MethodGet, "127.0.0.1:7710", "https://evil.example"),
	}
	for _, r := range accepted {
		if err := Check(r, true); err != nil {
			t.Errorf("%s from %q refused: %v", r.Method, r.Header.Get("Origin"), err)
		}
	}
	refused := []*http.Request{
		request(http.MethodPost, "127.0.0.1:7710", "https://evil.example"),
		request(http.MethodPost, "127.0.0.1:7710", "http://127.0.0.1:9999"),
		request(http.MethodDelete, "127.0.0.1:7710", "null"),
		request(http.MethodPatch, "127.0.0.1:7710", "file://"),
		request(http.MethodPost, "hub.internal:7710", "https://evil.example"),
	}
	for _, r := range refused {
		if err := Check(r, r.Host != "hub.internal:7710"); err == nil {
			t.Errorf("%s from %q to %q accepted", r.Method, r.Header.Get("Origin"), r.Host)
		}
	}
}
