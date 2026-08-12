// mockagent is a minimal ACP agent used for end-to-end testing of the
// gateway's ACP host without a real backend.
//
// Behavior: echoes each text prompt back as an agent message chunk. If the
// prompt contains the word "perm", it first requests permission from the
// client and reports the outcome.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"

	"github.com/gopact-ai/acp"
)

type agent struct {
	client  *acp.ClientCaller
	counter atomic.Int64
}

func (a *agent) Initialize(_ context.Context, _ *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	return &acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionV1,
		AgentInfo:         &acp.Implementation{Name: "mockagent", Version: "0.1.0"},
		AgentCapabilities: &acp.AgentCapabilities{LoadSession: true},
	}, nil
}

func (a *agent) NewSession(_ context.Context, _ *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	id := acp.SessionID(fmt.Sprintf("mock-session-%d", a.counter.Add(1)))
	return &acp.NewSessionResponse{SessionID: id}, nil
}

func (a *agent) LoadSession(_ context.Context, _ *acp.LoadSessionRequest) (*acp.LoadSessionResponse, error) {
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

	chunk := acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("echo: " + input))
	if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: chunk}); err != nil {
		return nil, err
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *agent) Cancel(_ context.Context, _ *acp.CancelNotification) error { return nil }

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
