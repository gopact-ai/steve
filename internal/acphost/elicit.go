package acphost

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/view"
)

// AskUserFunc puts a question from the agent in front of the user and returns
// what they chose. An empty answer means they declined or never replied.
type AskUserFunc func(context.Context, view.Question) (view.Answer, error)

// errUnsupportedForm marks a form Steve cannot honestly render — a number, a
// free-text box, a multi-select. Declining is the correct answer there:
// making something up would put words in the user's mouth, and rendering
// half the form would silently drop the rest.
var errUnsupportedForm = errors.New("elicitation form is not a single choice")

// CreateElicitation answers the agent's request to ask the user something.
//
// Steve advertises only the form mode, and within it only the single-select
// shape: one property whose values are enumerated. That is exactly what
// claude-agent-acp's AskUserQuestion sends, and it is the only shape a chat
// card can put in front of someone without inventing a UI for arbitrary JSON
// Schema.
func (ch *clientHandler) CreateElicitation(ctx context.Context, req *acp.CreateElicitationRequest) (*acp.CreateElicitationResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("elicitation/create: empty request")
	}
	if resp, decided := ch.decideMCPToolApproval(req); decided {
		return resp, nil
	}
	ch.h.mu.Lock()
	col := ch.h.collectors[req.SessionID]
	var ask AskUserFunc
	if col != nil && col.generation == ch.generation {
		ask = col.askUser
		if col.ctx != nil {
			ctx = col.ctx
		}
	}
	ch.h.mu.Unlock()
	if ask == nil {
		// No turn is listening — nobody can be asked, so say so rather than
		// leaving the agent blocked until its own timeout.
		resp := acp.CancelCreateElicitationResponse()
		return &resp, nil
	}
	if req.Mode != acp.CreateElicitationRequestTypeForm {
		resp := acp.DeclineCreateElicitationResponse()
		return &resp, nil
	}
	key, question, err := elicitQuestion(req)
	if err != nil {
		resp := acp.DeclineCreateElicitationResponse()
		return &resp, nil
	}
	answer, err := ask(ctx, question)
	if err != nil || !answer.Chosen() {
		resp := acp.CancelCreateElicitationResponse()
		return &resp, nil
	}
	value, err := acp.NewElicitationContentValue(answer.Value)
	if err != nil {
		resp := acp.CancelCreateElicitationResponse()
		return &resp, nil
	}
	resp := acp.AcceptCreateElicitationResponse()
	resp.Content = &map[string]acp.ElicitationContentValue{key: value}
	return &resp, nil
}

// isMCPToolApproval spots codex's "may I call this MCP tool" request.
// codex-acp wraps it as an elicitation whenever the client renders forms,
// carrying codex's own marker through _meta.
func isMCPToolApproval(req *acp.CreateElicitationRequest) bool {
	if req.Meta == nil {
		return false
	}
	if kind, _ := req.Meta["codex_approval_kind"].(string); kind == "mcp_tool_call" {
		return true
	}
	flag, _ := req.Meta["is_mcp_tool_approval"].(bool)
	return flag
}

// decideMCPToolApproval routes an MCP tool-call approval through the
// permission broker, exactly as if the agent had asked via
// session/request_permission. Elicitation is just the envelope codex-acp
// picked because Steve renders forms; which envelope arrived must not decide
// whether a human gets pulled in. Without this, calling the gateway's own
// injected messaging server parks the turn on a card nobody was asked to
// expect. Returns decided=false when the policy genuinely wants a human.
func (ch *clientHandler) decideMCPToolApproval(req *acp.CreateElicitationRequest) (*acp.CreateElicitationResponse, bool) {
	if !isMCPToolApproval(req) {
		return nil, false
	}
	broker := ch.h.cfg.Permission
	if broker.NeedsAsk(acp.ToolKindExecute) {
		return nil, false
	}
	if !broker.Allow(acp.ToolKindExecute) {
		log.Printf("acphost: mcp tool approval declined by policy")
		resp := acp.DeclineCreateElicitationResponse()
		return &resp, true
	}
	resp := acp.AcceptCreateElicitationResponse()
	if value, ok := persistChoice(req); ok {
		content := map[string]acp.ElicitationContentValue{"persist": value}
		resp.Content = &content
	}
	log.Printf("acphost: mcp tool approval accepted by policy")
	return &resp, true
}

// persistChoice picks the widest approval scope the form offers (session
// over once, never always), so an auto-approving policy answers once per
// session instead of once per call.
func persistChoice(req *acp.CreateElicitationRequest) (acp.ElicitationContentValue, bool) {
	property, ok := req.RequestedSchema.Properties["persist"]
	if !ok {
		return acp.ElicitationContentValue{}, false
	}
	choices := propertyChoices(property)
	for _, want := range []string{"session", "once"} {
		for _, choice := range choices {
			if choice.Value != want {
				continue
			}
			value, err := acp.NewElicitationContentValue(want)
			if err != nil {
				return acp.ElicitationContentValue{}, false
			}
			return value, true
		}
	}
	return acp.ElicitationContentValue{}, false
}

// elicitQuestion reduces a requested schema to the one choice Steve can ask,
// returning the property key the answer must come back under.
func elicitQuestion(req *acp.CreateElicitationRequest) (string, view.Question, error) {
	var key string
	var chosen acp.ElicitationPropertySchema
	for name, property := range req.RequestedSchema.Properties {
		choices := propertyChoices(property)
		if len(choices) == 0 {
			// A free-text companion field is optional by construction, so
			// ignoring it still leaves a form we can answer honestly.
			if isOptionalText(property) {
				continue
			}
			return "", view.Question{}, errUnsupportedForm
		}
		if key != "" {
			// Two real questions in one form; a card answers one.
			return "", view.Question{}, errUnsupportedForm
		}
		key, chosen = name, property
	}
	if key == "" {
		return "", view.Question{}, errUnsupportedForm
	}
	title := ""
	if chosen.Title != nil {
		title = *chosen.Title
	}
	return key, view.Question{
		Message: req.Message,
		Title:   title,
		Choices: propertyChoices(chosen),
	}, nil
}

// propertyChoices reads the enumerated values of a single-select property.
// ACP allows either the plain "enum" list or "oneOf" with per-value labels.
func propertyChoices(property acp.ElicitationPropertySchema) []view.Choice {
	if property.Type != acp.ElicitationPropertySchemaTypeString {
		return nil
	}
	if property.OneOf != nil {
		out := make([]view.Choice, 0, len(*property.OneOf))
		for _, option := range *property.OneOf {
			value := enumConst(option)
			if value == "" {
				continue
			}
			choice := view.Choice{Value: value, Label: value}
			if option.Title != "" {
				choice.Label = option.Title
			}
			if option.Description != nil {
				choice.Detail = *option.Description
			}
			out = append(out, choice)
		}
		return out
	}
	if property.Enum != nil {
		out := make([]view.Choice, 0, len(*property.Enum))
		for _, value := range *property.Enum {
			out = append(out, view.Choice{Value: value, Label: value})
		}
		return out
	}
	return nil
}

// isOptionalText spots the free-text box an agent offers beside its choices.
// claude-agent-acp sends one for "type your own answer instead"; it is never
// required, so leaving it unanswered is valid.
func isOptionalText(property acp.ElicitationPropertySchema) bool {
	return property.Type == acp.ElicitationPropertySchemaTypeString &&
		property.OneOf == nil && property.Enum == nil
}

func enumConst(option acp.EnumOption) string {
	if option.Const == "" {
		return ""
	}
	return option.Const
}
