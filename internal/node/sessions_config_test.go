package node

import (
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// A node restart may move its reverse messaging port. The native context
// resumed with the new port has the same configuration; nothing else about
// the servers may change.
func TestResumeAcceptsOnlyAMovedMessagingPort(t *testing.T) {
	server := func(name, url string) acp.MCPServer {
		return acp.HTTPMCPServer(name, url, []acp.HTTPHeader{{Name: "Authorization", Value: "Bearer test-token"}})
	}
	for _, tc := range []struct {
		name          string
		before, after acp.MCPServer
		accepted      bool
	}{
		{"moved port", server("steve", "http://127.0.0.1:20001/mcp"), server("steve", "http://127.0.0.1:20002/mcp"), true},
		{"moved IPv6 port", server("steve", "http://[::1]:20001/mcp"), server("steve", "http://[::1]:20002/mcp"), true},
		{"not loopback", server("steve", "http://10.0.0.1:20001/mcp"), server("steve", "http://10.0.0.1:20002/mcp"), false},
		{"named host", server("steve", "http://localhost:20001/mcp"), server("steve", "http://localhost:20002/mcp"), false},
		{"other host", server("steve", "http://127.0.0.1:20001/mcp"), server("steve", "http://127.0.0.2:20001/mcp"), false},
		{"other path", server("steve", "http://127.0.0.1:20001/mcp"), server("steve", "http://127.0.0.1:20002/other"), false},
		{"other server", server("files", "http://127.0.0.1:20001/mcp"), server("files", "http://127.0.0.1:20002/mcp"), false},
		{"sse", acp.SSEMCPServer("steve", "http://127.0.0.1:20001/mcp", nil), acp.SSEMCPServer("steve", "http://127.0.0.1:20002/mcp", nil), false},
		{"other token", server("steve", "http://127.0.0.1:20001/mcp"), acp.HTTPMCPServer("steve", "http://127.0.0.1:20002/mcp", []acp.HTTPHeader{{Name: "Authorization", Value: "Bearer other-token"}}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, req, old := resumedFixture(t, "/must-not-start")
			req.MCPServers = []acp.MCPServer{tc.before}
			old.ConfigHash = sessionConfigHash(req)
			req.MCPServers = []acp.MCPServer{tc.after}
			if err := validateResumeSource(req, old); (err == nil) != tc.accepted {
				t.Fatalf("resume accepted = %v, want %v: %v", err == nil, tc.accepted, err)
			}
			if req.MCPServers[0].URL != tc.after.URL {
				t.Fatal("hashing changed the address the agent is given")
			}
		})
	}
}

// A revoked credential can be replaced on the same resume that finds the
// messaging server on a new port.
func TestMCPAuthorizationRefreshAfterTheMessagingPortMoved(t *testing.T) {
	_, req, old := mcpRefreshFixture(t, "/must-not-start")
	req.MCPServers[0].URL = "http://127.0.0.1:2/mcp"
	if err := validateResumeSource(req, old); err != nil {
		t.Fatal(err)
	}
	req.MCPAuthorizationRefresh = &nodewire.MCPAuthorizationRefresh{PreviousAuthorization: "Bearer another-token"}
	if err := validateResumeSource(req, old); err == nil {
		t.Fatal("a wrong previous credential was accepted with a moved port")
	}
}
