package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/readmodel"
)

// A loopback bind keeps other machines out, not the pages open in the owner's
// browser: those can POST to 127.0.0.1, and a rebound DNS name can read it.
func TestLoopbackConsoleRefusesOtherSites(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	server := serve(t, model, ServerConfig{Token: testToken})
	console := &fakeConsole{}
	server.SetConsole(console)

	send := func(host, origin string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, server.URL()+"/console/send", strings.NewReader(`{"conversation":"console:main","input":"rm -rf"}`))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Authorization", "Bearer "+testToken)
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := send("", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d, want 403", code)
	}
	if code := send("evil.example:7710", ""); code != http.StatusForbidden {
		t.Fatalf("rebound host POST = %d, want 403", code)
	}
	if len(console.replies) != 0 {
		t.Fatalf("refused requests reached the console: %+v", console.replies)
	}

	read, _ := http.NewRequest(http.MethodGet, server.URL()+"/state", nil)
	read.Host = "evil.example:7710"
	read.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(read)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("rebound host read = %d, want 403", res.StatusCode)
	}

	if code := send("", server.URL()); code != http.StatusOK {
		t.Fatalf("same-origin POST = %d, want 200", code)
	}
}
