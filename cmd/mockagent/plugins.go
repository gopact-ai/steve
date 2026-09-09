package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gopact-ai/acp"
)

func (a *agent) exercisePlugins(ctx context.Context, session acp.SessionID) error {
	raw, _ := a.mcp.Load(string(session))
	servers, _ := raw.([]acp.MCPServer)
	var results []string
	for _, server := range servers {
		if !strings.HasPrefix(server.Name, "sp_") {
			continue
		}
		if _, _, err := a.mcpRPC(&server, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "mockagent", "version": "1"}}); err != nil {
			return err
		}
		text, failed, err := a.mcpTool(&server, "review", map[string]any{"request": "manual review"})
		if err != nil || failed {
			return fmt.Errorf("plugin tool failed: %s: %v", text, err)
		}
		results = append(results, server.Name+"="+text)
	}
	sort.Strings(results)
	return a.say(ctx, session, "[plugins: "+strings.Join(results, "; ")+"] ")
}
