package acphost

import (
	"context"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

func TestElicitationReachesTheUserAndAnswerReturns(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var asked view.Question
	out, _, err := h.PromptTurn(t.Context(), sid, generation, "askme please", nil, nil,
		func(_ context.Context, q view.Question) (view.Answer, error) {
			asked = q
			return view.Answer{Value: "Blue"}, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if asked.Message != "Which colour do you prefer?" {
		t.Errorf("message = %q", asked.Message)
	}
	if asked.Title != "Colour" {
		t.Errorf("title = %q, want the property's own title", asked.Title)
	}
	want := []view.Choice{
		{Value: "Red", Label: "Red", Detail: "You prefer red."},
		{Value: "Blue", Label: "Blue", Detail: "You prefer blue."},
	}
	if len(asked.Choices) != len(want) {
		t.Fatalf("choices = %+v", asked.Choices)
	}
	for i := range want {
		if asked.Choices[i] != want[i] {
			t.Errorf("choice %d = %+v, want %+v", i, asked.Choices[i], want[i])
		}
	}
	// The agent must receive the answer under the property key it asked with.
	if !strings.Contains(out, "[answer: accept:Blue]") {
		t.Fatalf("agent saw %q, want the accepted answer", out)
	}
}

// A turn with nobody to ask must not leave the agent blocked.
func TestElicitationWithoutAskerCancels(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := h.Prompt(t.Context(), sid, generation, "askme please", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[answer: cancel]") {
		t.Fatalf("agent saw %q, want a cancel", out)
	}
}

func TestElicitationDeclinedByUserCancels(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := h.PromptTurn(t.Context(), sid, generation, "askme please", nil, nil,
		func(context.Context, view.Question) (view.Answer, error) {
			return view.Answer{}, nil // user let it time out
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[answer: cancel]") {
		t.Fatalf("agent saw %q, want a cancel", out)
	}
}

// codex asks "may I call this MCP tool" through the same elicitation
// channel as real user questions. The permission broker must answer it, the
// way it would answer session/request_permission — a human is only pulled
// in when the policy actually wants one.
func TestMCPToolApprovalDecidedByPolicyNotHuman(t *testing.T) {
	h := newTestHost(t, "auto")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	out, _, err := h.PromptTurn(t.Context(), sid, generation, "mcpapprove now", nil, nil,
		func(context.Context, view.Question) (view.Answer, error) {
			asked = true
			return view.Answer{}, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if asked {
		t.Fatal("auto policy still pulled a human into an MCP tool approval")
	}
	if !strings.Contains(out, "[approval: accept:persist=session]") {
		t.Fatalf("agent saw %q, want a session-scoped acceptance", out)
	}
}

func TestMCPToolApprovalDeclinedByDenyPolicy(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	out, _, err := h.PromptTurn(t.Context(), sid, generation, "mcpapprove now", nil, nil,
		func(context.Context, view.Question) (view.Answer, error) {
			asked = true
			return view.Answer{}, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if asked {
		t.Fatal("deny policy still pulled a human into an MCP tool approval")
	}
	if !strings.Contains(out, "[approval: decline]") {
		t.Fatalf("agent saw %q, want a decline", out)
	}
}

func TestMCPToolApprovalReadPolicyAsksHuman(t *testing.T) {
	h := newTestHost(t, "read")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var asked view.Question
	out, _, err := h.PromptTurn(t.Context(), sid, generation, "mcpapprove now", nil, nil,
		func(_ context.Context, q view.Question) (view.Answer, error) {
			asked = q
			return view.Answer{Value: "once"}, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked.Choices) != 3 || !strings.Contains(asked.Message, "channel_send") {
		t.Fatalf("human question = %+v, want the approval choices", asked)
	}
	if !strings.Contains(out, "[approval: accept:persist=once]") {
		t.Fatalf("agent saw %q, want the human's choice", out)
	}
}
