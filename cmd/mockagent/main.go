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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
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
			PromptCapabilities: &acp.PromptCapabilities{Image: os.Getenv("MOCKAGENT_NO_MEDIA") == "", EmbeddedContext: os.Getenv("MOCKAGENT_NO_MEDIA") == ""},
			LoadSession:        true,
			MCPCapabilities:    &acp.MCPCapabilities{HTTP: true},
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

// Prompt runs the script the prompt's words select, in a fixed order: the
// media markers first, then each keyword's exchange with the client, then
// the echo and the stop reason. "ignore-cancel" and "slow" end the turn on
// their own terms and skip everything after them.
func (a *agent) Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
	input := promptText(req.Prompt)
	// MOCKAGENT_STREAM_CHUNKS unset or not a number means no streaming.
	if chunks, _ := strconv.Atoi(os.Getenv("MOCKAGENT_STREAM_CHUNKS")); chunks > 0 && chunks <= 1000 {
		return a.streamFragments(ctx, req.SessionID, chunks)
	}
	if err := a.echoMedia(ctx, req); err != nil {
		return nil, err
	}
	if strings.Contains(input, "ignore-cancel") {
		return a.ignoreCancel(ctx, req.SessionID)
	}
	if strings.Contains(input, "perm") {
		if err := a.requestPermission(ctx, req.SessionID); err != nil {
			return nil, err
		}
	}
	if strings.Contains(input, "slow") {
		return a.waitForCancel(ctx, req.SessionID)
	}
	if strings.Contains(input, "askme") {
		if err := a.askColour(ctx, req.SessionID, input); err != nil {
			return nil, err
		}
	}
	if strings.Contains(input, "mcpapprove") {
		if err := a.askMCPApproval(ctx, req.SessionID); err != nil {
			return nil, err
		}
	}
	// "mcpupdate" sends one milestone card and evolves it twice with
	// channel_update, the one-evolving-card shape the instructions steer
	// agents toward. Results are echoed for the wire test.
	if strings.Contains(input, "mcpupdate") {
		if err := a.say(ctx, req.SessionID, a.mcpUpdate(req.SessionID)+" "); err != nil {
			return nil, err
		}
	}
	// "mcpfull" exercises the gateway's built-in messaging MCP server the
	// way a real agent would: handshake, send a milestone, watch a mention
	// get refused, recall the milestone. The outcome is echoed so a wire
	// test can assert on it.
	if strings.Contains(input, "mcpfull") {
		if err := a.say(ctx, req.SessionID, a.mcpFull(req.SessionID)+" "); err != nil {
			return nil, err
		}
	}
	if strings.Contains(input, "plugincheck") {
		if err := a.exercisePlugins(ctx, req.SessionID); err != nil {
			return nil, err
		}
	}
	if strings.Contains(input, "plan") {
		if err := a.reportPlan(ctx, req.SessionID); err != nil {
			return nil, err
		}
	}
	if strings.Contains(input, "switchmodel") {
		if err := a.switchModel(ctx, req.SessionID); err != nil {
			return nil, err
		}
	}
	if err := a.say(ctx, req.SessionID, scriptedReply(input)); err != nil {
		return nil, err
	}
	return a.endTurn(ctx, req.SessionID, input)
}

// promptText is the prompt's text blocks joined, which is what the script
// keywords are looked for in.
func promptText(blocks []acp.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if block.Type == acp.ContentBlockTypeText {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

// say sends one agent message chunk to the client.
func (a *agent) say(ctx context.Context, sessionID acp.SessionID, text string) error {
	chunk := acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock(text))
	return a.client.Update(ctx, &acp.SessionNotification{SessionID: sessionID, Update: chunk})
}

// streamFragments answers with a fixed number of chunks and nothing else,
// for tests that measure streaming rather than content.
func (a *agent) streamFragments(ctx context.Context, sessionID acp.SessionID, chunks int) (*acp.PromptResponse, error) {
	for range chunks {
		if err := a.say(ctx, sessionID, "stream-fragment\n"); err != nil {
			return nil, err
		}
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// echoMedia reports every image and blob resource in the prompt as a
// marker with its size and digest, so a test can check what arrived.
func (a *agent) echoMedia(ctx context.Context, req *acp.PromptRequest) error {
	for _, block := range req.Prompt {
		encoded, mime, kind := "", "", ""
		if block.Type == acp.ContentBlockTypeImage {
			encoded, kind = block.Data, "image"
			if block.MIMEType != nil {
				mime = *block.MIMEType
			}
		}
		if block.Type == acp.ContentBlockTypeResource && block.Resource.Blob != nil {
			encoded, kind = *block.Resource.Blob, "resource"
			if block.Resource.MIMEType != nil {
				mime = *block.Resource.MIMEType
			}
		}
		if kind == "" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return err
		}
		if err := a.say(ctx, req.SessionID, fmt.Sprintf("[media: %s %s %d %x] ", kind, mime, len(data), sha256.Sum256(data))); err != nil {
			return err
		}
	}
	return nil
}

// ignoreCancel is an agent that never answers session/cancel: it keeps
// the turn open until the client gives up on it.
func (a *agent) ignoreCancel(ctx context.Context, sessionID acp.SessionID) (*acp.PromptResponse, error) {
	if err := a.say(ctx, sessionID, "still running"); err != nil {
		return nil, err
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// waitForCancel holds the turn until session/cancel arrives, then ends it
// with StopReasonCanceled the way a well-behaved agent does.
func (a *agent) waitForCancel(ctx context.Context, sessionID acp.SessionID) (*acp.PromptResponse, error) {
	stop := make(chan struct{})
	a.canceling.Store(string(sessionID), stop)
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

// requestPermission asks the client about one tool call and echoes what
// it decided.
func (a *agent) requestPermission(ctx context.Context, sessionID acp.SessionID) error {
	resp, err := a.client.RequestPermission(ctx, &acp.RequestPermissionRequest{
		SessionID: sessionID,
		ToolCall:  acp.ToolCallUpdate{ToolCallID: "tool-1", Title: ptr("dangerous operation")},
		Options: []acp.PermissionOption{
			{OptionID: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionID: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return err
	}
	return a.say(ctx, sessionID, fmt.Sprintf("[permission: %s/%s] ", resp.Outcome.Outcome, resp.Outcome.OptionID))
}

// askColour elicits a single-choice answer the way claude-agent-acp's
// AskUserQuestion does and echoes what came back.
func (a *agent) askColour(ctx context.Context, sessionID acp.SessionID, input string) error {
	title := "Colour"
	red, blue := "You prefer red.", "You prefer blue."
	schema := acp.ElicitationSchema{
		Type:     acp.ElicitationSchemaTypeObject,
		Required: &[]string{"question_0"},
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
	if strings.Contains(input, "askme-required-text") {
		schema.Required = &[]string{"question_0", "question_0_custom"}
	}
	ereq := acp.SessionFormCreateElicitationRequest("Which colour do you prefer?", schema, sessionID)
	resp, err := a.client.CreateElicitation(ctx, &ereq)
	if err != nil {
		return err
	}
	picked := string(resp.Action)
	if resp.Content != nil {
		if raw, ok := (*resp.Content)["question_0"]; ok {
			picked += ":" + strings.Trim(string(raw), `"`)
		}
	}
	return a.say(ctx, sessionID, "[answer: "+picked+"] ")
}

// askMCPApproval raises the elicitation codex-acp sends when codex wants
// approval for an MCP tool call: form mode, a "persist" scope choice, and
// the codex marker in _meta. The response is echoed so tests can see
// whether policy or a human answered.
func (a *agent) askMCPApproval(ctx context.Context, sessionID acp.SessionID) error {
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
	ereq := acp.SessionFormCreateElicitationRequest(`Allow tool channel_send on server "steve"?`, schema, sessionID)
	ereq.Meta = acp.Meta{"codex_approval_kind": "mcp_tool_call"}
	resp, err := a.client.CreateElicitation(ctx, &ereq)
	if err != nil {
		return err
	}
	picked := string(resp.Action)
	if resp.Content != nil {
		if raw, ok := (*resp.Content)["persist"]; ok {
			picked += ":persist=" + strings.Trim(string(raw), `"`)
		}
	}
	return a.say(ctx, sessionID, "[approval: "+picked+"] ")
}

// reportPlan sends a three-step plan already under way.
func (a *agent) reportPlan(ctx context.Context, sessionID acp.SessionID) error {
	steps := []acp.PlanEntry{
		{Content: "look around", Priority: acp.PlanEntryPriorityHigh, Status: acp.PlanEntryStatusCompleted},
		{Content: "do the thing", Priority: acp.PlanEntryPriorityMedium, Status: acp.PlanEntryStatusInProgress},
		{Content: "check it", Priority: acp.PlanEntryPriorityLow, Status: acp.PlanEntryStatusPending},
	}
	return a.client.Update(ctx, &acp.SessionNotification{SessionID: sessionID, Update: acp.PlanSessionUpdate(steps)})
}

// switchModel reports a mid-turn model change the way a real agent does.
func (a *agent) switchModel(ctx context.Context, sessionID acp.SessionID) error {
	swap := acp.ConfigOptionUpdateSessionUpdate([]acp.SessionConfigOption{modelOption("mock-deep")})
	return a.client.Update(ctx, &acp.SessionNotification{SessionID: sessionID, Update: swap})
}

// scriptedReply is the turn's answer: an echo, unless the prompt is a
// planning brief, a verifier's brief or a step meant to upset the plan.
// A planning brief gets a plan, so the supervisor loop can be driven end
// to end without a real model. The plan is deliberately shaped to exercise
// placement across machines and a fan-in: two branches needing different
// capabilities, then a merge. Wording is read from the brief so the same
// mock can answer a revision (it keeps the finished step ids).
func scriptedReply(input string) string {
	switch {
	case strings.Contains(input, "输出一份 JSON 计划"):
		return mockPlan(input)
	case strings.Contains(input, "你是审核者"):
		// A verifier's verdict: fail anything whose goal asks to be failed,
		// so a test can drive the verification edge on real hosts.
		if strings.Contains(input, "reject me") {
			return "FAIL\nthe artifact is not where it was claimed to be"
		}
		return "PASS"
	case strings.Contains(input, "surprise"):
		// A step that learns something which makes the plan wrong.
		return "echo: " + input + "\nFINDING: noted in passing\nREPLAN: the target now requires a recheck before shipping"
	}
	return "echo: " + input
}

// endTurn builds the response, reporting usage and a cancelled stop when
// the prompt asks for them.
func (a *agent) endTurn(ctx context.Context, sessionID acp.SessionID, input string) (*acp.PromptResponse, error) {
	resp := &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}
	if strings.Contains(input, "reportusage") {
		// Keep context/cost on the notification and tokens on the response
		// so tests exercise the two independent channels real adapters use.
		update := acp.UsageUpdateSessionUpdate(1600, 128000)
		update.Cost = &acp.Cost{Amount: 0.125, Currency: "USD"}
		if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: sessionID, Update: update}); err != nil {
			return nil, err
		}
		resp.Usage = &acp.Usage{
			TotalTokens: 300, InputTokens: 100, OutputTokens: 110,
			ThoughtTokens: ptr(uint64(30)), CachedReadTokens: ptr(uint64(40)), CachedWriteTokens: ptr(uint64(50)),
		}
	}
	if strings.Contains(input, "cancelme") {
		resp.StopReason = acp.StopReasonCanceled
	}
	return resp, nil
}

// SetSessionConfigOption accepts any listed model and answers with the
// revised list, the way an agent that does not notify separately would.
func (a *agent) SetSessionConfigOption(_ context.Context, req *acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
	value, _ := req.Value.(acp.SessionConfigValueID) // any other shape is "" and refused below as an unknown model
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
	// An untouched atomic.Value holds nil, which reads as "" here: nothing deleted yet, so both sessions are listed.
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

// steveServer is the session's "steve" HTTP MCP server, or the bracketed
// line the tests read when the session has none.
func (a *agent) steveServer(sessionID acp.SessionID) (*acp.MCPServer, string) {
	raw, ok := a.mcp.Load(string(sessionID))
	if !ok {
		return nil, "[mcp: no server config]"
	}
	servers, _ := raw.([]acp.MCPServer) // only NewSession and LoadSession store here, always this type
	for i := range servers {
		if servers[i].Name == "steve" && servers[i].Type == acp.MCPServerTypeHTTP {
			return &servers[i], ""
		}
	}
	return nil, "[mcp: no steve server]"
}

// mcpFull runs the send/deny/recall sequence against the session's "steve"
// HTTP MCP server and reports what happened in one bracketed line.
func (a *agent) mcpFull(sessionID acp.SessionID) string {
	target, missing := a.steveServer(sessionID)
	if target == nil {
		return missing
	}
	if _, _, err := a.mcpRPC(target, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mockagent", "version": "0.1.0"},
	}); err != nil {
		return "[mcp: initialize failed: " + err.Error() + "]"
	}
	sent, isError, err := a.mcpTool(target, "channel_send", map[string]any{"content": "## milestone\nphase one done"})
	if err != nil || isError {
		return fmt.Sprintf("[mcp: send failed err=%v text=%s]", err, sent)
	}
	id := strings.TrimPrefix(sent, "sent message_id=")
	_, mentionRejected, err := a.mcpTool(target, "channel_send", map[string]any{"content": "hi", "mention": true})
	if err != nil {
		return "[mcp: mention call failed: " + err.Error() + "]"
	}
	recalled, recallErr, err := a.mcpTool(target, "channel_recall", map[string]any{"message_id": id})
	if err != nil || recallErr {
		return fmt.Sprintf("[mcp: recall failed err=%v text=%s]", err, recalled)
	}
	return fmt.Sprintf("[mcp: sent=%s mention_rejected=%v recalled_ok=true]", id, mentionRejected)
}

func (a *agent) mcpUpdate(sessionID acp.SessionID) string {
	target, missing := a.steveServer(sessionID)
	if target == nil {
		return missing
	}
	sent, isError, err := a.mcpTool(target, "channel_send", map[string]any{"content": "progress v1", "progress": "1/2"})
	if err != nil || isError {
		return fmt.Sprintf("[mcp: send failed err=%v text=%s]", err, sent)
	}
	id := strings.TrimPrefix(sent, "sent message_id=")
	steps := []map[string]any{
		{"message_id": id, "content": "progress v2"},
		{"message_id": id, "content": "progress v3 final", "progress": "2/2"},
	}
	for _, step := range steps {
		out, isError, err := a.mcpTool(target, "channel_update", step)
		if err != nil || isError {
			return fmt.Sprintf("[mcp: update failed err=%v text=%s]", err, out)
		}
	}
	return fmt.Sprintf("[mcp: card=%s updated_twice=true]", id)
}

func (a *agent) mcpTool(server *acp.MCPServer, name string, args map[string]any) (string, bool, error) {
	result, _, err := a.mcpRPC(server, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", false, err
	}
	isError, _ := result["isError"].(bool) // absent or not a bool is the JSON-RPC default for a result that did not fail
	text := ""
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		if first, ok := content[0].(map[string]any); ok {
			text, _ = first["text"].(string) // a text that is not a string reads as empty, which the callers report
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

// mockPlan answers a planning brief. It picks two capabilities it sees on
// offer in the brief and fans out across them, so on a real fleet the steps
// land on different machines. On a revision — the brief lists what already
// ran — it keeps those ids and adds one more step, which is enough to show
// the finished work being reused.
func mockPlan(brief string) string {
	// Prefer the capabilities that live on other machines: the point of
	// the fixture is to show steps spreading across the fleet, and "basic"
	// is what the hub itself offers.
	caps := []string{}
	for _, want := range []string{"gpu", "internal-net", "prod-cred"} {
		if strings.Contains(brief, want) {
			caps = append(caps, want)
		}
	}
	if len(caps) == 0 && strings.Contains(brief, "basic") {
		caps = []string{"basic"}
	}
	if len(caps) == 0 {
		caps = []string{"any"}
	}
	first, second := caps[0], caps[len(caps)-1]
	if strings.Contains(brief, "上一版计划跑到了哪") {
		return `{"steps":[` +
			`{"id":"build","goal":"say built","requires":["` + first + `"],"verify":{"kind":"none","why":"mock"}},` +
			`{"id":"stage","goal":"say staged","requires":["` + second + `"],"needs":["build"],"verify":{"kind":"none","why":"mock"}},` +
			`{"id":"recheck","goal":"say rechecked after revision","requires":["` + second + `"],"needs":["stage"],"verify":{"kind":"none","why":"mock"}},` +
			`{"id":"ship","goal":"say shipped","requires":["` + second + `"],"merge":["recheck"],"verify":{"kind":"none","why":"mock"}}]}`
	}
	return `{"steps":[` +
		`{"id":"build","goal":"say built","requires":["` + first + `"],"verify":{"kind":"none","why":"mock"}},` +
		`{"id":"stage","goal":"say staged","requires":["` + second + `"],"verify":{"kind":"none","why":"mock"}},` +
		`{"id":"ship","goal":"say shipped","requires":["` + second + `"],"merge":["build","stage"],"verify":{"kind":"none","why":"mock"}}]}`
}
