package httpapi

import (
	"errors"
	"net"
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

// The Host check follows the address the console is bound to, not how the
// configuration spells it: each of these listens on loopback, so a rebound
// Host is refused even when the page has the owner's token.
func TestLoopbackBoundConsoleRefusesReboundHostsHoweverItIsNamed(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	for _, addr := range []string{"LOCALHOST:0", "localhost.:0", "foo.localhost:0"} {
		t.Run(addr, func(t *testing.T) {
			server, err := NewServer(model, ServerConfig{Addr: addr, Token: testToken})
			var unresolved *net.DNSError
			if errors.As(err, &unresolved) {
				t.Skipf("this platform does not resolve %s: %v", addr, err)
			}
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = server.Serve() }()
			t.Cleanup(func() { _ = server.Close() })
			if code := readState(t, server.URL(), "evil.example:7710"); code != http.StatusForbidden {
				t.Errorf("rebound host read = %d, want 403", code)
			}
			if code := readState(t, server.URL(), ""); code != http.StatusOK {
				t.Errorf("read on the console's own address = %d, want 200", code)
			}
		})
	}
}

// A console bound beyond loopback cannot tell a rebound name from one the
// operator gave it, so it answers any Host and its token keeps others out.
func TestNetworkBoundConsoleAnswersAnyHostWithTheToken(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	server := serve(t, model, ServerConfig{Addr: "0.0.0.0:0", Token: testToken})
	url := strings.NewReplacer("0.0.0.0", "127.0.0.1", "[::]", "127.0.0.1").Replace(server.URL())
	if code := readState(t, url, "evil.example:7710"); code != http.StatusOK {
		t.Fatalf("read through another name = %d, want 200", code)
	}
}

// readState reads /state from the console at url with the owner's token,
// naming host in the request when it is not empty.
func readState(t *testing.T, url, host string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/state", nil)
	if host != "" {
		req.Host = host
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}
