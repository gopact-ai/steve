// mockagent is a minimal ACP agent used for end-to-end testing of the
// gateway's ACP host without a real backend.
//
// Behavior: echoes each text prompt back as an agent message chunk. If the
// prompt contains the word "perm", it first requests permission from the
// client and reports the outcome. If it contains "switchmodel", it reports a
// mid-turn model change the way a real agent does. If it contains "plan", it
// reports a three-step plan and then advances it. If it contains "askme", it
// elicits a single-choice answer from the user the way claude-agent-acp's
// AskUserQuestion does, and echoes what came back.
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
		ProtocolVersion: acp.ProtocolVersionV1,
		AgentInfo:       &acp.Implementation{Name: "mockagent", Version: "0.1.0"},
		AgentCapabilities: &acp.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: &acp.SessionCapabilities{
				List: &acp.SessionListCapabilities{},
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

func (a *agent) NewSession(_ context.Context, _ *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	id := acp.SessionID(fmt.Sprintf("mock-session-%d", a.counter.Add(1)))
	return &acp.NewSessionResponse{
		SessionID:     id,
		ConfigOptions: &[]acp.SessionConfigOption{modeOption("agent"), modelOption("mock-fast")},
	}, nil
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

func (a *agent) ListSessions(_ context.Context, _ *acp.ListSessionsRequest) (*acp.ListSessionsResponse, error) {
	return &acp.ListSessionsResponse{Sessions: []acp.SessionInfo{
		{SessionID: "mock-session-1", Cwd: "/tmp"},
	}}, nil
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
