package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gopact-ai/steve/internal/clientcap"
)

const clientInstructions = `## Client capabilities
- steve_client_capabilities lists the authenticated online clients and the capabilities explicitly granted to this conversation. Use only capabilities actually returned by that tool.
- steve_client_invoke returns an invocation ID. Reuse its idempotency_key and identical arguments when a request is uncertain; changing the key can duplicate an external action.
- Read steve_client_result(id) until the invocation reaches done, failed, cancelled or unknown. approval means the user must approve it in the Inbox. Do not claim success before a done receipt.
- An offline or unknown client outcome is not permission to replay the action, run a shell command on another client, or broaden a grant. Ask the user to reconcile the original invocation.`

type ClientInvoker interface {
	Available(context.Context, Binding) ([]clientcap.Registration, error)
	Invoke(context.Context, Binding, ClientInvoke) (clientcap.Invocation, error)
	Result(context.Context, Binding, string) (clientcap.Invocation, error)
}
type ClientInvoke struct {
	Key        string          `json:"idempotency_key"`
	Client     string          `json:"client"`
	Capability string          `json:"capability"`
	Arguments  json.RawMessage `json:"arguments"`
}

func (s *Server) SetClientInvoker(invoker ClientInvoker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientInvoker = invoker
}
func (s *Server) clientCall(ctx context.Context, b binding, tool string, raw json.RawMessage) (string, error) {
	if b.taskID != "" || b.delegatedBy != "" {
		return "", errors.New("client capabilities are scoped to the owner's private conversation")
	}
	if _, ok := ScopeFromContext(ctx); !ok {
		return "", errors.New("client capabilities require a fixed authorized execution")
	}
	s.mu.Lock()
	invoker := s.clientInvoker
	s.mu.Unlock()
	if invoker == nil {
		return "", errors.New("client capabilities are unavailable")
	}
	var out any
	var err error
	switch tool {
	case "steve_client_capabilities":
		if err = decodeScheduleArgs(raw, &struct{}{}); err == nil {
			out, err = invoker.Available(ctx, publicBinding(b))
		}
	case "steve_client_invoke":
		var input ClientInvoke
		if err = decodeScheduleArgs(raw, &input); err == nil {
			out, err = invoker.Invoke(ctx, publicBinding(b), input)
		}
	case "steve_client_result":
		var input struct {
			ID string `json:"id"`
		}
		if err = decodeScheduleArgs(raw, &input); err == nil {
			out, err = invoker.Result(ctx, publicBinding(b), input.ID)
		}
	}
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(out)
	return string(body), err
}
func clientTools() []map[string]any {
	return []map[string]any{
		{"name": "steve_client_capabilities", "description": "List authenticated online client capabilities granted to this private conversation.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
		{"name": "steve_client_invoke", "description": "Request one granted client capability with a stable idempotency key; returns a durable invocation, not execution success.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"idempotency_key": map[string]any{"type": "string"}, "client": map[string]any{"type": "string"}, "capability": map[string]any{"type": "string"}, "arguments": map[string]any{"type": "object"}}, "required": []string{"idempotency_key", "client", "capability", "arguments"}, "additionalProperties": false}},
		{"name": "steve_client_result", "description": "Read the original client invocation's receipt. Unknown outcomes require user reconciliation.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}},
	}
}
