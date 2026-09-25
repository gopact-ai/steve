package capability

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
)

// Steve's messaging server may listen on a new port after a restart. The
// session keeps the same server, so neither fingerprint may change; the
// agent must still be given the real address.
func TestFingerprintsIgnoreOnlyThePlatformServerPort(t *testing.T) {
	selected := agent.Agent{ID: "worker"}
	extra := func(url, token string, platform bool) []Extra {
		return []Extra{{Name: "steve", Platform: platform, Instructions: "platform guidance", Server: MCPServer{
			Type: "http", URL: url, Headers: map[string]string{"Authorization": "Bearer " + token},
		}}}
	}
	assemble := func(extras []Extra) Capabilities {
		t.Helper()
		got, err := NewAssembler(nil).AssembleExtra(selected, home.ModeNone, extras)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := assemble(extra("http://127.0.0.1:20001/mcp", "token", true))
	for _, tc := range []struct {
		name   string
		extras []Extra
		same   bool
	}{
		{"moved port", extra("http://127.0.0.1:20002/mcp", "token", true), true},
		{"other host", extra("http://127.0.0.2:20001/mcp", "token", true), false},
		{"other path", extra("http://127.0.0.1:20001/other", "token", true), false},
		{"other token", extra("http://127.0.0.1:20001/mcp", "renewed", true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := assemble(tc.extras)
			if same := got.Fingerprint == base.Fingerprint && got.SessionFingerprint == base.SessionFingerprint; same != tc.same {
				t.Fatalf("fingerprints unchanged = %v, want %v", same, tc.same)
			}
			if got.MCPServers[0].URL != tc.extras[0].Server.URL {
				t.Fatalf("agent is given %q, want the real address %q", got.MCPServers[0].URL, tc.extras[0].Server.URL)
			}
		})
	}
	// A configured server's port is its identity.
	if moved := assemble(extra("http://127.0.0.1:20002/mcp", "token", false)); moved.SessionFingerprint == assemble(extra("http://127.0.0.1:20001/mcp", "token", false)).SessionFingerprint {
		t.Fatal("a configured server's port change kept the session fingerprint")
	}
	v6 := assemble(extra("http://[::1]:20001/mcp", "token", true))
	if moved := assemble(extra("http://[::1]:20002/mcp", "token", true)); moved.SessionFingerprint != v6.SessionFingerprint || v6.SessionFingerprint == base.SessionFingerprint {
		t.Fatal("IPv6 loopback lost its host or kept its port in the fingerprint")
	}
}
