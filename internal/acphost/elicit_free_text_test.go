package acphost

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

func TestElicitationTextRoundTripPreservesTheRequestedProperty(t *testing.T) {
	textProperty := acp.StringElicitationPropertySchema()
	textTitle := "Recovery plan"
	textProperty.Title = &textTitle
	choices := acp.StringElicitationPropertySchema()
	choices.Enum = &[]string{"continue", "wait"}
	for _, test := range []struct {
		name     string
		schema   acp.ElicitationSchema
		answer   view.Answer
		content  map[string]string
		wantText bool
	}{
		{"text", acp.ElicitationSchema{Properties: map[string]acp.ElicitationPropertySchema{"plan": textProperty}, Required: &[]string{"plan"}}, view.Answer{Text: "Use another node.\nKeep the workspace."}, map[string]string{"plan": "Use another node.\nKeep the workspace."}, true},
		{"claude-custom", acp.ElicitationSchema{Properties: map[string]acp.ElicitationPropertySchema{"question_0": choices, "question_0_custom": textProperty}}, view.Answer{Text: "I will reconnect the service."}, map[string]string{"question_0_custom": "I will reconnect the service."}, true},
		{"claude-choice", acp.ElicitationSchema{Properties: map[string]acp.ElicitationPropertySchema{"question_0": choices, "question_0_custom": textProperty}}, view.Answer{Value: "wait"}, map[string]string{"question_0": "wait"}, true},
		{"required-choice", acp.ElicitationSchema{Properties: map[string]acp.ElicitationPropertySchema{"choice": choices, "context": textProperty}, Required: &[]string{"choice"}}, view.Answer{Value: "continue"}, map[string]string{"choice": "continue"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := acp.SessionFormCreateElicitationRequest("I tried reconnecting. The service is unavailable. How should I continue?", test.schema, "")
			resp, asked := elicitationRoundTrip(t, req, "read", test.answer)
			if resp.Action != acp.CreateElicitationResponseTypeAccept || resp.Content == nil {
				t.Fatalf("agent received %+v", resp)
			}
			if asked.AllowFreeText != test.wantText || asked.Message != req.Message || asked.SessionID != "elicitation-session" || asked.Generation == 0 {
				t.Fatalf("question = %+v", asked)
			}
			got := map[string]string{}
			for key, value := range *resp.Content {
				var text string
				if err := json.Unmarshal(value, &text); err != nil {
					t.Fatal(err)
				}
				got[key] = text
			}
			if !reflect.DeepEqual(got, test.content) {
				t.Fatalf("accepted content = %+v, want %+v", got, test.content)
			}
		})
	}
}

func TestElicitationInvalidTextAndDecisionsDoNotAuthorize(t *testing.T) {
	choices := acp.StringElicitationPropertySchema()
	choices.Enum = &[]string{"once", "session"}
	for _, test := range []struct {
		name       string
		properties map[string]acp.ElicitationPropertySchema
		answer     view.Answer
		permission bool
	}{
		{"blank", map[string]acp.ElicitationPropertySchema{"text": acp.StringElicitationPropertySchema()}, view.Answer{Text: " \n\t"}, false},
		{"oversized", map[string]acp.ElicitationPropertySchema{"text": acp.StringElicitationPropertySchema()}, view.Answer{Text: strings.Repeat("x", (64<<10)+1)}, false},
		{"both", map[string]acp.ElicitationPropertySchema{"choice": choices, "custom": acp.StringElicitationPropertySchema()}, view.Answer{Value: "once", Text: "session"}, false},
		{"unknown", map[string]acp.ElicitationPropertySchema{"choice": choices}, view.Answer{Value: "once", Decision: "unknown"}, false},
		{"decline-with-value", map[string]acp.ElicitationPropertySchema{"choice": choices}, view.Answer{Value: "once", Decision: "decline"}, false},
		{"permission", map[string]acp.ElicitationPropertySchema{"persist": choices, "custom": acp.StringElicitationPropertySchema()}, view.Answer{Text: "once"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := acp.SessionFormCreateElicitationRequest("Please decide.", acp.ElicitationSchema{Properties: test.properties}, "")
			if test.permission {
				req.Meta = acp.Meta{"is_mcp_tool_approval": true}
			}
			resp, asked := elicitationRoundTrip(t, req, "read", test.answer)
			if resp.Action != acp.CreateElicitationResponseTypeCancel || resp.Content != nil {
				t.Fatalf("invalid answer reached agent: %+v", resp)
			}
			if test.permission && (asked.AllowFreeText || asked.Kind != "permission") {
				t.Fatalf("permission exposed text authorization: %+v", asked)
			}
		})
	}
}

func TestElicitationRejectsFormsItCannotFullyAsk(t *testing.T) {
	choices := acp.StringElicitationPropertySchema()
	choices.Enum = &[]string{"a", "b"}
	pattern := "^[a-z]+$"
	constrained := acp.StringElicitationPropertySchema()
	constrained.Pattern = &pattern
	for _, schema := range []acp.ElicitationSchema{
		{Properties: map[string]acp.ElicitationPropertySchema{"a": acp.StringElicitationPropertySchema(), "b": acp.StringElicitationPropertySchema()}},
		{Properties: map[string]acp.ElicitationPropertySchema{"a": choices, "b": choices}},
		{Properties: map[string]acp.ElicitationPropertySchema{"text": constrained}},
		{Properties: map[string]acp.ElicitationPropertySchema{"text": acp.StringElicitationPropertySchema()}, Required: &[]string{"missing"}},
		{Properties: map[string]acp.ElicitationPropertySchema{"choice": choices, "custom": acp.StringElicitationPropertySchema()}, Required: &[]string{"custom"}},
	} {
		req := acp.SessionFormCreateElicitationRequest("Please decide.", schema, "")
		resp, asked := elicitationRoundTrip(t, req, "read", view.Answer{Text: "answer"})
		if resp.Action != acp.CreateElicitationResponseTypeDecline || asked.SessionID != "" {
			t.Fatalf("unsupported schema was partially rendered: %+v, %+v", resp, asked)
		}
	}
}

func TestElicitationUnknownOrStaleSessionCannotAutoApprove(t *testing.T) {
	broker, err := permission.New("auto")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Permission: broker})
	h.collectors["old-session"] = &collector{generation: 1, ctx: t.Context()}
	h.collectors["cancelled-session"] = &collector{generation: 2, ctx: t.Context()}
	closed, cancel := context.WithCancel(t.Context())
	cancel()
	h.collectors["cancelled-session"].ctx = closed
	ch := &clientHandler{h: h, generation: 2}
	for _, sid := range []acp.SessionID{"unknown-session", "old-session", "cancelled-session"} {
		req := acp.SessionFormCreateElicitationRequest("Approve?", acp.ElicitationSchema{}, sid)
		req.Meta = acp.Meta{"is_mcp_tool_approval": true}
		resp, err := ch.CreateElicitation(t.Context(), &req)
		if err != nil || resp == nil || resp.Action != acp.CreateElicitationResponseTypeCancel {
			t.Fatalf("stale callback %s = %+v, %v", sid, resp, err)
		}
	}
}

// This transport keeps a real ACP stream and reverse request isolated from
// installed agents, credentials and workspaces; only the agent's answer is fake.
type elicitationTestTransport struct{ request acp.CreateElicitationRequest }

func (elicitationTestTransport) Name() string { return "elicitation-test" }

func (tr elicitationTestTransport) Start(context.Context) (Process, error) {
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	conn, err := acp.NewAgent(input, output, func(client *acp.ClientCaller) acp.AgentHandler {
		return &elicitationTestAgent{client: client, request: tr.request}
	})
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, err
	}
	return &elicitationTestProcess{conn: conn, stdin: stdin, stdout: stdout, output: output}, nil
}

type elicitationTestProcess struct {
	conn   *acp.Conn
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	output *io.PipeWriter
}

func (p *elicitationTestProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *elicitationTestProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *elicitationTestProcess) Wait() error {
	<-p.conn.Done()
	return p.output.Close()
}
func (p *elicitationTestProcess) Kill() {
	_ = p.conn.Close()
	_ = p.stdin.Close()
	_ = p.output.Close()
	_ = p.stdout.Close()
}

type elicitationTestAgent struct {
	client  *acp.ClientCaller
	request acp.CreateElicitationRequest
}

func (*elicitationTestAgent) Initialize(context.Context, *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	return &acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionV1}, nil
}
func (*elicitationTestAgent) NewSession(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	return &acp.NewSessionResponse{SessionID: "elicitation-session"}, nil
}
func (*elicitationTestAgent) Cancel(context.Context, *acp.CancelNotification) error { return nil }
func (a *elicitationTestAgent) Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
	a.request.SessionID = req.SessionID
	resp, err := a.client.CreateElicitation(ctx, &a.request)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	if err := a.client.Update(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock(string(raw)))}); err != nil {
		return nil, err
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func elicitationRoundTrip(t *testing.T, req acp.CreateElicitationRequest, policy string, answer view.Answer) (acp.CreateElicitationResponse, view.Question) {
	t.Helper()
	broker, err := permission.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Transport: elicitationTestTransport{request: req}, Permission: broker})
	t.Cleanup(h.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sid, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var asked view.Question
	out, _, err := h.PromptTurn(ctx, sid, generation, "continue", nil, nil, func(_ context.Context, q view.Question) (view.Answer, error) {
		asked = q
		return answer, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp acp.CreateElicitationResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("agent response %q: %v", out, err)
	}
	return resp, asked
}
