package node

import (
	"slices"
	"strings"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// previousMCPRequest reconstructs the complete source request without
// changing the request sent to the native agent. No other field is replaceable.
func previousMCPRequest(req nodewire.SessionRequest) (nodewire.SessionRequest, error) {
	refuse := func() (nodewire.SessionRequest, error) {
		return nodewire.SessionRequest{}, sessionError("forbidden", "invalid built-in MCP authorization refresh")
	}
	proof := req.MCPAuthorizationRefresh
	if proof == nil || !validMCPBearer(proof.PreviousAuthorization) {
		return refuse()
	}
	serverIndex := -1
	for i, server := range req.MCPServers {
		if server.Name == nodewire.PlatformMCPServer {
			if serverIndex != -1 {
				return refuse()
			}
			serverIndex = i
		}
	}
	if serverIndex == -1 {
		return refuse()
	}
	server := req.MCPServers[serverIndex]
	if server.Type != acp.MCPServerTypeHTTP {
		return refuse()
	}
	headerIndex := -1
	for i, header := range server.Headers {
		if strings.EqualFold(header.Name, "Authorization") {
			if headerIndex != -1 {
				return refuse()
			}
			headerIndex = i
		}
	}
	if headerIndex == -1 {
		return refuse()
	}
	current := server.Headers[headerIndex].Value
	if !validMCPBearer(current) || current[7:] == proof.PreviousAuthorization[7:] {
		return refuse()
	}
	req.MCPServers = slices.Clone(req.MCPServers)
	req.MCPServers[serverIndex].Headers = slices.Clone(server.Headers)
	req.MCPServers[serverIndex].Headers[headerIndex].Value = proof.PreviousAuthorization
	return req, nil
}

func validMCPBearer(value string) bool {
	if len(value) <= 7 || !strings.EqualFold(value[:7], "Bearer ") {
		return false
	}
	token := value[7:]
	padding := false
	for i, c := range token {
		if c == '=' {
			if i == 0 {
				return false
			}
			padding = true
			continue
		}
		if padding || !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~+/", c)) {
			return false
		}
	}
	return true
}
