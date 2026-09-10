package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gopact-ai/acp"
)

func (a *agent) tool(ctx context.Context, id acp.SessionID, s *session, name string, args any) (string, error) {
	var server *acp.MCPServer
	for i := range s.servers {
		if s.servers[i].Name == "steve" && s.servers[i].Type == acp.MCPServerTypeHTTP {
			server = &s.servers[i]
			break
		}
	}
	if server == nil {
		return "", errors.New("Steve did not supply the session MCP endpoint")
	}
	call := acp.ToolCallID(fmt.Sprintf("lab-tool-%d", a.sequence.Add(1)))
	update := acp.ToolCallSessionUpdate(call, name)
	running := acp.ToolCallStatusInProgress
	update.Status = &running
	update.RawInput = args
	if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: id, Update: update}); err != nil {
		return "", err
	}
	if _, err := rpc(ctx, server, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "fleetlab", "version": "1"}}); err != nil {
		return "", err
	}
	result, err := rpc(ctx, server, "tools/call", map[string]any{"name": name, "arguments": args})
	var reply struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err == nil {
		err = json.Unmarshal(result, &reply)
	}
	var text string
	for _, c := range reply.Content {
		text += c.Text
	}
	if err == nil && reply.IsError {
		err = errors.New(text)
	}
	finished := acp.ToolCallUpdateSessionUpdate(call)
	status := acp.ToolCallStatusCompleted
	if err != nil {
		status = acp.ToolCallStatusFailed
	}
	finished.Status = &status
	finished.RawOutput = text
	if updateErr := a.client.Update(ctx, &acp.SessionNotification{SessionID: id, Update: finished}); err == nil {
		err = updateErr
	}
	return text, err
}

func rpc(ctx context.Context, server *acp.MCPServer, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range server.Headers {
		req.Header.Set(h.Name, h.Value)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MCP HTTP %s", resp.Status)
	}
	var envelope struct {
		Result json.RawMessage           `json:"result"`
		Error  *struct{ Message string } `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return nil, err
	}
	if envelope.Error != nil {
		return nil, errors.New(envelope.Error.Message)
	}
	return envelope.Result, nil
}
