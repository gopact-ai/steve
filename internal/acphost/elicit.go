package acphost

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/view"
)

// AskUserFunc puts a question from the agent in front of the user and returns
// their choice or written reply. An empty answer means they never replied.
type AskUserFunc func(context.Context, view.Question) (view.Answer, error)

// errUnsupportedForm marks forms that need more than one choice or plain-text
// reply. Declining avoids inventing values or silently omitting required fields.
var errUnsupportedForm = errors.New("elicitation form is not a supported question")

// CreateElicitation answers the agent's request to ask the user something.
//
// Supported forms contain one enum, one plain string, or an enum with an
// optional text alternative. Permission approvals still require an enum.
func (ch *clientHandler) CreateElicitation(ctx context.Context, req *acp.CreateElicitationRequest) (*acp.CreateElicitationResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("elicitation/create: empty request")
	}
	ch.h.mu.Lock()
	col := ch.h.collectors[req.SessionID]
	var ask AskUserFunc
	active := col != nil && col.generation == ch.generation
	if active {
		ask = col.askUser
		if col.ctx != nil {
			ctx = col.ctx
		}
	}
	ch.h.mu.Unlock()
	if !active || ctx.Err() != nil {
		resp := acp.CancelCreateElicitationResponse()
		return &resp, nil
	}
	if resp, decided := ch.decideMCPToolApproval(req); decided {
		return resp, nil
	}
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
	form, question, err := elicitQuestion(req)
	if err != nil {
		resp := acp.DeclineCreateElicitationResponse()
		return &resp, nil
	}
	if isMCPToolApproval(req) {
		question.Kind = "permission"
		question.AllowFreeText = false
		if len(question.Choices) == 0 {
			resp := acp.DeclineCreateElicitationResponse()
			return &resp, nil
		}
	}
	question.SessionID, question.Generation = string(req.SessionID), ch.generation
	answer, err := ask(ctx, question)
	if err != nil || !answer.Chosen() || (answer.Decision != "" && answer.Decision != "accept") {
		if err == nil && !answer.Chosen() && answer.Decision == "decline" {
			resp := acp.DeclineCreateElicitationResponse()
			return &resp, nil
		}
		resp := acp.CancelCreateElicitationResponse()
		return &resp, nil
	}
	key, text := form.choiceKey, answer.Value
	if answer.Text != "" {
		if !question.AllowFreeText || answer.Value != "" || strings.TrimSpace(answer.Text) == "" || len(answer.Text) > 64<<10 {
			resp := acp.CancelCreateElicitationResponse()
			return &resp, nil
		}
		key, text = form.textKey, answer.Text
	} else if !slices.ContainsFunc(question.Choices, func(choice view.Choice) bool { return choice.Value == answer.Value }) {
		resp := acp.CancelCreateElicitationResponse()
		return &resp, nil
	}
	value, err := acp.NewElicitationContentValue(text)
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

type elicitationForm struct {
	choiceKey string
	textKey   string
}

// elicitQuestion retains the original property keys so text alternatives are
// returned as text, without fabricating a value for an unselected enum.
func elicitQuestion(req *acp.CreateElicitationRequest) (elicitationForm, view.Question, error) {
	unsupported := func() (elicitationForm, view.Question, error) {
		return elicitationForm{}, view.Question{}, errUnsupportedForm
	}
	if req.RequestedSchema.Type != "" && req.RequestedSchema.Type != acp.ElicitationSchemaTypeObject {
		return unsupported()
	}
	required := func(name string) bool {
		return req.RequestedSchema.Required != nil && slices.Contains(*req.RequestedSchema.Required, name)
	}
	if req.RequestedSchema.Required != nil {
		for _, name := range *req.RequestedSchema.Required {
			if _, ok := req.RequestedSchema.Properties[name]; !ok {
				return unsupported()
			}
		}
	}
	var form elicitationForm
	for name, property := range req.RequestedSchema.Properties {
		if name == "" || !isSupportedString(property) {
			return unsupported()
		}
		if property.Enum != nil || property.OneOf != nil {
			if form.choiceKey != "" || len(propertyChoices(property)) == 0 {
				return unsupported()
			}
			form.choiceKey = name
		} else {
			if form.textKey != "" {
				return unsupported()
			}
			form.textKey = name
		}
	}
	key := form.choiceKey
	if key == "" {
		key = form.textKey
	} else if form.textKey != "" && required(form.textKey) {
		// Both fields could be required; this UI only submits one answer.
		return unsupported()
	}
	if key == "" {
		return unsupported()
	}
	chosen := req.RequestedSchema.Properties[key]
	title := ""
	if chosen.Title != nil {
		title = *chosen.Title
	}
	return form, view.Question{
		Kind: "question", Required: required(key),
		Message:       req.Message,
		Title:         title,
		Choices:       propertyChoices(chosen),
		AllowFreeText: form.textKey != "" && (form.choiceKey == "" || !required(form.choiceKey)),
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

// String constraints require validation and a way to explain those constraints
// before accepting a reply; do not present them as unrestricted text inputs.
func isSupportedString(property acp.ElicitationPropertySchema) bool {
	return property.Type == acp.ElicitationPropertySchemaTypeString &&
		property.MinLength == nil && property.MaxLength == nil && property.Pattern == nil && property.Format == nil && len(property.Fields) == 0
}

func enumConst(option acp.EnumOption) string {
	if option.Const == "" {
		return ""
	}
	return option.Const
}
