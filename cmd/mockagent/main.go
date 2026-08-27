// mockagent is a minimal ACP agent used for end-to-end testing of the
// gateway's ACP host without a real backend.
//
// Behavior: echoes each text prompt back as an agent message chunk. If the
// prompt contains the word "perm", it first requests permission from the
// client and reports the outcome. If it contains "switchmodel", it reports a
// mid-turn model change the way a real agent does. If it contains "plan", it
// reports a three-step plan and then advances it. If it contains "askme", it
// elicits a single-choice answer from the user the way claude-agent-acp's
// AskUserQuestion does, and echoes what came back. If it contains "slow", it
// waits until the client cancels the session and then ends the turn with
// StopReasonCanceled, the way a well-behaved agent answers session/cancel.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/acp"
)

type agent struct {
	client    *acp.ClientCaller
	counter   atomic.Int64
	deleted   atomic.Value
	canceling sync.Map
	// mcp remembers each session's MCP server config, the way a real agent
	// holds on to it to connect.
	mcp sync.Map
}

func (a *agent) Initialize(_ context.Context, _ *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	return &acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionV1,
		AgentInfo:       &acp.Implementation{Name: "mockagent", Version: "0.1.0"},
		AgentCapabilities: &acp.AgentCapabilities{
			LoadSession:     true,
			MCPCapabilities: &acp.MCPCapabilities{HTTP: true},
			SessionCapabilities: &acp.SessionCapabilities{
				List:   &acp.SessionListCapabilities{},
				Delete: &acp.SessionDeleteCapabilities{},
			},
		},
	}, nil
}

// modelOption mirrors the shape codex-acp and claude-agent-acp both use for
// their model selector: an opaque current value, plus the names to show for
// it.
func modelOption(current string) acp.SessionConfigOption {
	category := acp.SessionConfigOptionCategoryModel
	return acp.SessionConfigOption{
		Type: acp.SessionConfigOptionTypeSelect, ID: "model", Name: "Model", Category: &category,
		CurrentValue: acp.SessionConfigValueID(current),
		Options: acp.SessionConfigSelectOptions{Ungrouped: &acp.UngroupedSessionConfigSelectOptions{
			{Value: "mock-fast", Name: "Mock Fast"},
			{Value: "mock-deep", Name: "Mock Deep"},
		}},
	}
}

func modeOption(current string) acp.SessionConfigOption {
	category := acp.SessionConfigOptionCategoryMode
	return acp.SessionConfigOption{
		Type: acp.SessionConfigOptionTypeSelect, ID: "mode", Name: "Mode", Category: &category,
		CurrentValue: acp.SessionConfigValueID(current),
		Options: acp.SessionConfigSelectOptions{Ungrouped: &acp.UngroupedSessionConfigSelectOptions{
			{Value: "read-only", Name: "Read-only"},
			{Value: "agent", Name: "Agent"},
		}},
	}
}

func (a *agent) NewSession(_ context.Context, req *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	id := acp.SessionID(fmt.Sprintf("mock-session-%d", a.counter.Add(1)))
	a.mcp.Store(string(id), req.MCPServers)
	return &acp.NewSessionResponse{
		SessionID:     id,
		ConfigOptions: &[]acp.SessionConfigOption{modeOption("agent"), modelOption("mock-fast")},
	}, nil
}

func (a *agent) LoadSession(_ context.Context, req *acp.LoadSessionRequest) (*acp.LoadSessionResponse, error) {
	a.mcp.Store(string(req.SessionID), req.MCPServers)
	return &acp.LoadSessionResponse{}, nil
}

func (a *agent) Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
	var text strings.Builder
	for _, block := range req.Prompt {
		if block.Type == acp.ContentBlockTypeText {
			text.WriteString(block.Text)
		}
	}
	input := text.String()

	if strings.Contains(input, "perm") {
		resp, err := a.client.RequestPermission(ctx, &acp.RequestPermissionRequest{
			SessionID: req.SessionID,
			ToolCall:  acp.ToolCallUpdate{ToolCallID: "tool-1", Title: ptr("dangerous operation")},
			Options: []acp.PermissionOption{
				{OptionID: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionID: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return nil, err
		}
		note := acp.AgentMessageChunkSessionUpdate(
			acp.TextContentBlock(fmt.Sprintf("[permission: %s/%s] ", resp.Outcome.Outcome, resp.Outcome.OptionID)))
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: note}); err != nil {
			return nil, err
		}
	}

	if strings.Contains(input, "slow") {
		stop := make(chan struct{})
		a.canceling.Store(string(req.SessionID), stop)
		select {
		case <-stop:
			// Answering the prompt is what tells the client the session is
			// still consistent; a real agent that just went quiet here would
			// leave it unusable.
			return &acp.PromptResponse{StopReason: acp.StopReasonCanceled}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if strings.Contains(input, "askme") {
		title := "Colour"
		red, blue := "You prefer red.", "You prefer blue."
		schema := acp.ElicitationSchema{
			Type: acp.ElicitationSchemaTypeObject,
			Properties: map[string]acp.ElicitationPropertySchema{
				"question_0": {
					Type: acp.ElicitationPropertySchemaTypeString, Title: &title,
					OneOf: &[]acp.EnumOption{
						{Const: "Red", Title: "Red", Description: &red},
						{Const: "Blue", Title: "Blue", Description: &blue},
					},
				},
				// The free-text companion claude-agent-acp sends beside its
				// choices; a client that cannot render it may skip it.
				"question_0_custom": {Type: acp.ElicitationPropertySchemaTypeString},
			},
		}
		req := acp.SessionFormCreateElicitationRequest("Which colour do you prefer?", schema, req.SessionID)
		resp, err := a.client.CreateElicitation(ctx, &req)
		if err != nil {
			return nil, err
		}
		picked := string(resp.Action)
		if resp.Content != nil {
			if raw, ok := (*resp.Content)["question_0"]; ok {
				picked += ":" + strings.Trim(string(raw), `"`)
			}
		}
		note := acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("[answer: " + picked + "] "))
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: note}); err != nil {
			return nil, err
		}
	}

	// "mcpapprove" raises the elicitation codex-acp sends when codex wants
	// approval for an MCP tool call: form mode, a "persist" scope choice,
	// and the codex marker in _meta. The response is echoed so tests can
	// see whether policy or a human answered.
	if strings.Contains(input, "mcpapprove") {
		schema := acp.ElicitationSchema{
			Type: acp.ElicitationSchemaTypeObject,
			Properties: map[string]acp.ElicitationPropertySchema{
				"persist": {Type: acp.ElicitationPropertySchemaTypeString, OneOf: &[]acp.EnumOption{
					{Const: "once", Title: "Approve once"},
					{Const: "session", Title: "Approve for session"},
					{Const: "always", Title: "Always approve"},
				}},
			},
		}
		ereq := acp.SessionFormCreateElicitationRequest(`Allow tool feishu_send on server "feishu"?`, schema, req.SessionID)
		ereq.Meta = acp.Meta{"codex_approval_kind": "mcp_tool_call"}
		resp, err := a.client.CreateElicitation(ctx, &ereq)
		if err != nil {
			return nil, err
		}
		picked := string(resp.Action)
		if resp.Content != nil {
			if raw, ok := (*resp.Content)["persist"]; ok {
				picked += ":persist=" + strings.Trim(string(raw), `"`)
			}
		}
		note := acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("[approval: " + picked + "] "))
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: note}); err != nil {
			return nil, err
		}
	}

	// "mcpfull" exercises the gateway's built-in messaging MCP server the
	// way a real agent would: handshake, send a milestone, watch a mention
	// get refused, recall the milestone. The outcome is echoed so a wire
	// test can assert on it.
	if strings.Contains(input, "mcpfull") {
		note := acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock(a.mcpFull(req.SessionID) + " "))
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: note}); err != nil {
			return nil, err
		}
	}

	if strings.Contains(input, "plan") {
		steps := []acp.PlanEntry{
			{Content: "look around", Priority: acp.PlanEntryPriorityHigh, Status: acp.PlanEntryStatusCompleted},
			{Content: "do the thing", Priority: acp.PlanEntryPriorityMedium, Status: acp.PlanEntryStatusInProgress},
			{Content: "check it", Priority: acp.PlanEntryPriorityLow, Status: acp.PlanEntryStatusPending},
		}
		if err := a.client.Update(ctx, &acp.SessionNotification{
			SessionID: req.SessionID, Update: acp.PlanSessionUpdate(steps),
		}); err != nil {
			return nil, err
		}
	}

	if strings.Contains(input, "switchmodel") {
		swap := acp.ConfigOptionUpdateSessionUpdate([]acp.SessionConfigOption{modelOption("mock-deep")})
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: swap}); err != nil {
			return nil, err
		}
	}

	chunk := acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("echo: " + input))
	if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: chunk}); err != nil {
		return nil, err
	}
	if strings.Contains(input, "cancelme") {
		return &acp.PromptResponse{StopReason: acp.StopReasonCanceled}, nil
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// SetSessionConfigOption accepts any listed model and answers with the
// revised list, the way an agent that does not notify separately would.
func (a *agent) SetSessionConfigOption(_ context.Context, req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
	value, _ := req.Value.(acp.SessionConfigValueID)
	if req.ConfigID != "model" {
		return nil, fmt.Errorf("unknown config option %q", req.ConfigID)
	}
	if value != "mock-fast" && value != "mock-deep" {
		return nil, fmt.Errorf("unknown model %q", value)
	}
	return &acp.SetSessionConfigOptionResponse{
		ConfigOptions: []acp.SessionConfigOption{modeOption("agent"), modelOption(string(value))},
	}, nil
}

func (a *agent) DeleteSession(_ context.Context, req *acp.DeleteSessionRequest) (*acp.DeleteSessionResponse, error) {
	a.deleted.Store(string(req.SessionID))
	return &acp.DeleteSessionResponse{}, nil
}

func (a *agent) ListSessions(_ context.Context, _ *acp.ListSessionsRequest) (*acp.ListSessionsResponse, error) {
	if gone, _ := a.deleted.Load().(string); gone == "mock-session-1" {
		return &acp.ListSessionsResponse{}, nil
	}
	return &acp.ListSessionsResponse{Sessions: []acp.SessionInfo{
		{SessionID: "mock-session-1", Cwd: "/tmp"},
	}}, nil
}

func (a *agent) Cancel(_ context.Context, n *acp.CancelNotification) error {
	if ch, ok := a.canceling.Load(string(n.SessionID)); ok {
		close(ch.(chan struct{}))
		a.canceling.Delete(string(n.SessionID))
	}
	return nil
}

// mcpFull runs the send/deny/recall sequence against the session's "feishu"
// HTTP MCP server and reports what happened in one bracketed line.
func (a *agent) mcpFull(sessionID acp.SessionID) string {
	raw, ok := a.mcp.Load(string(sessionID))
	if !ok {
		return "[mcp: no server config]"
	}
	servers, _ := raw.([]acp.MCPServer)
	var target *acp.MCPServer
	for i := range servers {
		if servers[i].Name == "feishu" && servers[i].Type == acp.MCPServerTypeHTTP {
			target = &servers[i]
			break
		}
	}
	if target == nil {
		return "[mcp: no feishu server]"
	}
	if _, _, err := a.mcpRPC(target, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mockagent", "version": "0.1.0"},
	}); err != nil {
		return "[mcp: initialize failed: " + err.Error() + "]"
	}
	sent, isError, err := a.mcpTool(target, "feishu_send", map[string]any{"content": "## milestone\nphase one done"})
	if err != nil || isError {
		return fmt.Sprintf("[mcp: send failed err=%v text=%s]", err, sent)
	}
	id := strings.TrimPrefix(sent, "sent message_id=")
	mention, mentionRejected, err := a.mcpTool(target, "feishu_send", map[string]any{"content": "hi", "mention": true})
	if err != nil {
		return "[mcp: mention call failed: " + err.Error() + "]"
	}
	_ = mention
	recalled, recallErr, err := a.mcpTool(target, "feishu_recall", map[string]any{"message_id": id})
	if err != nil || recallErr {
		return fmt.Sprintf("[mcp: recall failed err=%v text=%s]", err, recalled)
	}
	return fmt.Sprintf("[mcp: sent=%s mention_rejected=%v recalled_ok=true]", id, mentionRejected)
}

func (a *agent) mcpTool(server *acp.MCPServer, name string, args map[string]any) (string, bool, error) {
	result, _, err := a.mcpRPC(server, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", false, err
	}
	isError, _ := result["isError"].(bool)
	text := ""
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		if first, ok := content[0].(map[string]any); ok {
			text, _ = first["text"].(string)
		}
	}
	return text, isError, nil
}

func (a *agent) mcpRPC(server *acp.MCPServer, method string, params map[string]any) (map[string]any, int, error) {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, header := range server.Headers {
		req.Header.Set(header.Name, header.Value)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, resp.StatusCode, err
	}
	if envelope.Error != nil {
		return nil, resp.StatusCode, fmt.Errorf("rpc %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	return envelope.Result, resp.StatusCode, nil
}

func ptr[T any](v T) *T { return &v }

func main() {
	conn, err := acp.NewAgent(os.Stdin, os.Stdout, func(client *acp.ClientCaller) acp.AgentHandler {
		return &agent{client: client}
	})
	if err != nil {
		log.Fatalf("mockagent: %v", err)
	}
	defer conn.Close()
	<-conn.Done()
	if err := conn.Err(); err != nil && !errors.Is(err, io.EOF) {
		log.Fatalf("mockagent: %v", err)
	}
}
