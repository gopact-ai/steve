package acphost

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/view"
)

// sessionState is what Steve remembers about one agent session between turns.
// The agent reports its configuration when the session opens and revises it
// with config_option_update / current_mode_update notifications, so this
// outlives any single collector and is read back on every progress snapshot.
type sessionState struct {
	mu       sync.Mutex
	options  []acp.SessionConfigOption
	modes    []acp.SessionMode
	modeID   acp.SessionModeID
	commands []acp.AvailableCommand
}

// newSessionState files away what the agent reported when the session opened:
// the modes it offers, which one is current, and the selectors (model,
// effort, …) it exposes. Session updates revise all of it later.
func newSessionState(modes *acp.SessionModeState, options *[]acp.SessionConfigOption) *sessionState {
	state := &sessionState{}
	state.setModes(modes)
	if options != nil {
		state.setOptions(*options)
	}
	return state
}

// setOptions replaces the reported selectors. config_option_update carries
// the whole list rather than a delta, so replacing is the correct merge.
func (s *sessionState) setOptions(options []acp.SessionConfigOption) {
	// Missing metadata is not a replacement. A present empty full list is.
	if options == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.options = append(s.options[:0:0], options...)
	// The mode arrives on two channels; keep modeID the single answer to
	// "which mode" so current_mode_update and config_option_update agree.
	if opt, ok := findOption(s.options, acp.SessionConfigOptionCategoryMode); ok {
		if value, ok := selectValue(opt); ok {
			s.modeID = acp.SessionModeID(value)
		}
	}
}

func (s *sessionState) setModes(modes *acp.SessionModeState) {
	if modes == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modes = append(s.modes[:0:0], modes.AvailableModes...)
	if modes.CurrentModeID != "" {
		s.modeID = modes.CurrentModeID
	}
}

func (s *sessionState) setMode(id acp.SessionModeID) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modeID = id
}

func (s *sessionState) setCommands(commands []acp.AvailableCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands[:0:0], commands...)
}

func (s *sessionState) settings() view.Settings {
	if s == nil {
		return view.Settings{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return view.Settings{
		Model: s.modelLabel(), Models: s.modelNames(), Mode: s.modeLabel(),
		Options: optionsView(s.currentOptionsLocked()),
	}
}

// modelLabel resolves the model selector to the name a human would recognise.
// The current value is an opaque ID ("gpt-5.6-sol", "opus[1m]"); the readable
// name lives beside it in the option list.
func (s *sessionState) modelLabel() string {
	opt, ok := findOption(s.options, acp.SessionConfigOptionCategoryModel)
	if !ok {
		return ""
	}
	value, ok := selectValue(opt)
	if !ok {
		return ""
	}
	if name := selectName(opt, value); name != "" {
		return name
	}
	return value
}

// modelNames lists every model the selector offers, by display name, in
// the order the agent gave them. This is what the fleet's model column is
// made of once a harness has been seen running.
func (s *sessionState) modelNames() []string {
	opt, ok := findOption(s.options, acp.SessionConfigOptionCategoryModel)
	if !ok {
		return nil
	}
	var names []string
	add := func(choice acp.SessionConfigSelectOption) {
		if choice.Name != "" {
			names = append(names, choice.Name)
		} else {
			names = append(names, string(choice.Value))
		}
	}
	if opt.Options.Ungrouped != nil {
		for _, choice := range *opt.Options.Ungrouped {
			add(choice)
		}
	}
	if opt.Options.Groups != nil {
		for _, group := range *opt.Options.Groups {
			for _, choice := range group.Options {
				add(choice)
			}
		}
	}
	return names
}

func (s *sessionState) modeLabel() string {
	if s.modeID == "" {
		return ""
	}
	for _, mode := range s.modes {
		if mode.ID == s.modeID {
			return mode.Name
		}
	}
	if opt, ok := findOption(s.options, acp.SessionConfigOptionCategoryMode); ok {
		if name := selectName(opt, string(s.modeID)); name != "" {
			return name
		}
	}
	return string(s.modeID)
}

// findOption locates a selector by its semantic category. ACP calls category
// advisory and lets agents omit it, so fall back to matching the option ID —
// the reserved category names double as the conventional IDs.
func findOption(options []acp.SessionConfigOption, category acp.SessionConfigOptionCategory) (acp.SessionConfigOption, bool) {
	for _, opt := range options {
		if opt.Type == acp.SessionConfigOptionTypeSelect && opt.Category != nil && *opt.Category == category {
			return opt, true
		}
	}
	for _, opt := range options {
		if opt.Type == acp.SessionConfigOptionTypeSelect && string(opt.ID) == string(category) {
			return opt, true
		}
	}
	return acp.SessionConfigOption{}, false
}

// selectValue reads a select option's current value. Boolean options carry a
// bool instead; neither model nor mode is ever boolean, so anything but a
// string simply means "not a selector we can label".
func selectValue(opt acp.SessionConfigOption) (string, bool) {
	if opt.Type != acp.SessionConfigOptionTypeSelect {
		return "", false
	}
	switch value := opt.CurrentValue.(type) {
	case acp.SessionConfigValueID:
		return string(value), value != ""
	case string:
		return value, value != ""
	default:
		return "", false
	}
}

// selectName maps a value ID to the name the agent gave it, looking through
// both the flat and the grouped option layouts ACP allows.
func selectName(opt acp.SessionConfigOption, value string) string {
	if opt.Options.Ungrouped != nil {
		for _, choice := range *opt.Options.Ungrouped {
			if string(choice.Value) == value {
				return choice.Name
			}
		}
	}
	if opt.Options.Groups != nil {
		for _, group := range *opt.Options.Groups {
			for _, choice := range group.Options {
				if string(choice.Value) == value {
					return choice.Name
				}
			}
		}
	}
	return ""
}

// planSteps converts an agent's plan into the channel-neutral shape. ACP
// sends the whole plan on every revision, so the result replaces whatever
// came before rather than merging into it.
func planSteps(entries []acp.PlanEntry) []view.Step {
	if len(entries) == 0 {
		return nil
	}
	steps := make([]view.Step, 0, len(entries))
	for _, entry := range entries {
		steps = append(steps, view.Step{Text: entry.Content, Status: stepStatus(entry.Status)})
	}
	return steps
}

func stepStatus(status acp.PlanEntryStatus) view.StepStatus {
	switch status {
	case acp.PlanEntryStatusInProgress:
		return view.StepInProgress
	case acp.PlanEntryStatusCompleted:
		return view.StepCompleted
	default:
		return view.StepPending
	}
}

// Options lists the selectors the agent exposes for a session, so a caller
// can show what is changeable and what the current value is.
func (h *Host) Options(sid acp.SessionID) []acp.SessionConfigOption {
	h.mu.Lock()
	state := h.sessions[sid]
	h.mu.Unlock()
	if state == nil {
		return nil
	}
	return state.currentOptions()
}

// currentOptions is a copy of the reported selectors with the mode selector
// showing the mode actually in force. The mode moves on two channels, and a
// current_mode_update (or a session/set_mode the host issued itself) does
// not revise the option list; without this the selector would still claim
// the mode the session opened in, and a preference for the real one would
// look already applied.
func (s *sessionState) currentOptions() []acp.SessionConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentOptionsLocked()
}

func (s *sessionState) currentOptionsLocked() []acp.SessionConfigOption {
	out := append([]acp.SessionConfigOption(nil), s.options...)
	if s.modeID == "" {
		return out
	}
	for i := range out {
		isMode := (out[i].Category != nil && *out[i].Category == acp.SessionConfigOptionCategoryMode) || string(out[i].ID) == string(acp.SessionConfigOptionCategoryMode)
		if isMode && out[i].Type == acp.SessionConfigOptionTypeSelect {
			out[i].CurrentValue = acp.SessionConfigValueID(s.modeID)
			break
		}
	}
	return out
}

// ModelChoices lists the models this session can switch to, current first
// only if the agent listed it first. Empty when the agent exposes no model
// selector.
func (h *Host) ModelChoices(sid acp.SessionID) (acp.SessionConfigID, []view.Choice) {
	opt, ok := findOption(h.Options(sid), acp.SessionConfigOptionCategoryModel)
	if !ok {
		return "", nil
	}
	var out []view.Choice
	if opt.Options.Ungrouped != nil {
		for _, choice := range *opt.Options.Ungrouped {
			out = append(out, view.Choice{Value: string(choice.Value), Label: choice.Name})
		}
	}
	if opt.Options.Groups != nil {
		for _, group := range *opt.Options.Groups {
			for _, choice := range group.Options {
				out = append(out, view.Choice{Value: string(choice.Value), Label: group.Name + " · " + choice.Name})
			}
		}
	}
	return opt.ID, out
}

// optionsView renders every supported option, including opaque categories.
// Its string current values preserve the preference contract; Type carries
// the distinction between a select ID "false" and the boolean false.
func optionsView(options []acp.SessionConfigOption) []view.Option {
	var out []view.Option
	for _, opt := range options {
		if opt.Type != acp.SessionConfigOptionTypeSelect && opt.Type != acp.SessionConfigOptionTypeBoolean {
			continue
		}
		o := view.Option{ID: string(opt.ID), Name: opt.Name, Type: string(opt.Type)}
		if opt.Category != nil {
			o.Category = string(*opt.Category)
		}
		if opt.Type == acp.SessionConfigOptionTypeBoolean {
			if value, ok := opt.CurrentValue.(bool); ok {
				o.Current = strconv.FormatBool(value)
			}
			out = append(out, o)
			continue
		}
		if value, ok := selectValue(opt); ok {
			o.Current = value
		}
		if opt.Options.Ungrouped != nil {
			for _, choice := range *opt.Options.Ungrouped {
				o.Choices = append(o.Choices, view.Choice{Value: string(choice.Value), Label: choice.Name})
			}
		}
		if opt.Options.Groups != nil {
			for _, group := range *opt.Options.Groups {
				for _, choice := range group.Options {
					o.Choices = append(o.Choices, view.Choice{Value: string(choice.Value), Label: group.Name + " · " + choice.Name})
				}
			}
		}
		out = append(out, o)
	}
	return out
}

// SetOption proposes a value using the reported descriptor's wire type.
// Only the agent's full response or config_option_update changes Actual.
func (h *Host) SetOption(ctx context.Context, sid acp.SessionID, generation uint64, id acp.SessionConfigID, value string) error {
	h.mu.Lock()
	if err := h.SessionBlockedLocked(sid); err != nil {
		h.mu.Unlock()
		return err
	}
	if h.generation != generation {
		h.mu.Unlock()
		return fmt.Errorf("agent process changed before set option")
	}
	state := h.sessions[sid]
	if h.caller == nil || state == nil || !h.alive {
		h.mu.Unlock()
		return fmt.Errorf("session %q is not open", sid)
	}
	// Validate and reserve against the same original state. A lifecycle RPC
	// must not slip between descriptor lookup and configuration dispatch.
	req, err := configOptionRequest(sid, state.currentOptions(), id, value)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	op, err := h.beginConfigOperationLocked(ctx, sid, generation)
	caller := h.caller
	h.mu.Unlock()
	if err != nil {
		return err
	}

	// Do not hold the Host lock across I/O: an existing prompt and reverse
	// callbacks must continue while this SID is reserved against new sources.
	resp, err := caller.SetSessionConfigOption(ctx, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		return fmt.Errorf("session/set_config_option: %w", h.finishConfigOperationLocked(ctx, sid, op, err))
	}
	var confirmationErr error
	if resp == nil || resp.ConfigOptions == nil {
		confirmationErr = fmt.Errorf("session/set_config_option: agent omitted the required configOptions confirmation")
	} else {
		op.state.setOptions(resp.ConfigOptions)
	}
	// A matched response with missing confirmation is a local validation
	// error, not an unanswered RPC. Settle the reservation without inventing
	// Actual or reclassifying that response as a transport-unknown outcome.
	if err := h.finishConfigOperationLocked(ctx, sid, op, nil); err != nil {
		return fmt.Errorf("session/set_config_option: %w", err)
	}
	return confirmationErr
}

func configOptionRequest(sid acp.SessionID, options []acp.SessionConfigOption, id acp.SessionConfigID, value string) (acp.SetSessionConfigOptionRequest, error) {
	var descriptor *acp.SessionConfigOption
	for i := range options {
		if options[i].ID == id {
			descriptor = &options[i]
			break
		}
	}
	if descriptor == nil {
		return acp.SetSessionConfigOptionRequest{}, fmt.Errorf("session %q exposes no config option %q", sid, id)
	}
	switch descriptor.Type {
	case acp.SessionConfigOptionTypeSelect:
		return acp.ValueIDSetSessionConfigOptionRequest(sid, id, acp.SessionConfigValueID(value)), nil
	case acp.SessionConfigOptionTypeBoolean:
		if string(id) == "model" || string(id) == "mode" ||
			descriptor.Category != nil && (*descriptor.Category == acp.SessionConfigOptionCategoryModel || *descriptor.Category == acp.SessionConfigOptionCategoryMode) {
			return acp.SetSessionConfigOptionRequest{}, fmt.Errorf("config option %q: model and mode require a select descriptor", id)
		}
		if value != "true" && value != "false" {
			return acp.SetSessionConfigOptionRequest{}, fmt.Errorf("config option %q: boolean value must be \"true\" or \"false\"", id)
		}
		return acp.BooleanSetSessionConfigOptionRequest(sid, id, value == "true"), nil
	default:
		return acp.SetSessionConfigOptionRequest{}, fmt.Errorf("config option %q has unsupported type %q", id, descriptor.Type)
	}
}

// ListSessions asks the agent which sessions it still holds. Steve's own
// record can outlive the agent's. Listing is discovery, not proof that an
// unlisted session cannot be resumed.
func (h *Host) ListSessions(ctx context.Context) ([]acp.SessionInfo, error) {
	if err := h.ensureStarted(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	caller, capabilities := h.caller, h.capabilities
	h.mu.Unlock()
	if caller == nil {
		return nil, ErrClosed
	}
	if capabilities == nil || capabilities.SessionCapabilities == nil || capabilities.SessionCapabilities.List == nil {
		return nil, ErrListUnsupported
	}
	var out []acp.SessionInfo
	var cursor *string
	seen := map[string]bool{}
	for {
		resp, err := caller.ListSessions(ctx, &acp.ListSessionsRequest{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("session/list: %w", err)
		}
		out = append(out, resp.Sessions...)
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			return out, nil
		}
		if seen[*resp.NextCursor] {
			return nil, fmt.Errorf("session/list: repeated pagination cursor")
		}
		seen[*resp.NextCursor] = true
		cursor = resp.NextCursor
	}
}

// DeleteSession asks the agent to remove a session from its list. This does
// not confirm data erasure or native resource cleanup. Unsupported deletion
// is explicit and never removes Steve's session bookkeeping.
func (h *Host) DeleteSession(ctx context.Context, sid acp.SessionID) error {
	return h.deleteSession(ctx, sid)
}
