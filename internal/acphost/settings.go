package acphost

import (
	"context"
	"fmt"
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
	if len(options) == 0 {
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
	return view.Settings{Model: s.modelLabel(), Mode: s.modeLabel()}
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
		if opt.Category != nil && *opt.Category == category {
			return opt, true
		}
	}
	for _, opt := range options {
		if string(opt.ID) == string(category) {
			return opt, true
		}
	}
	return acp.SessionConfigOption{}, false
}

// selectValue reads a select option's current value. Boolean options carry a
// bool instead; neither model nor mode is ever boolean, so anything but a
// string simply means "not a selector we can label".
func selectValue(opt acp.SessionConfigOption) (string, bool) {
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
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]acp.SessionConfigOption(nil), state.options...)
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

// SetOption changes one of the agent's selectors. The agent confirms with a
// config_option_update, which is what actually moves Steve's own record, so
// this does not write the new value locally: an agent that refuses or
// substitutes a value stays the authority on what it is running.
func (h *Host) SetOption(ctx context.Context, sid acp.SessionID, generation uint64, id acp.SessionConfigID, value string) error {
	h.mu.Lock()
	if h.generation != generation {
		h.mu.Unlock()
		return fmt.Errorf("agent process changed before set option")
	}
	caller := h.caller
	known := h.sessions[sid] != nil
	h.mu.Unlock()
	if caller == nil || !known {
		return fmt.Errorf("session %q is not open", sid)
	}
	req := acp.ValueIDSetSessionConfigOptionRequest(sid, id, acp.SessionConfigValueID(value))
	resp, err := caller.SetSessionConfigOption(ctx, &req)
	if err != nil {
		return fmt.Errorf("session/set_config_option: %w", err)
	}
	// Some agents answer with the full revised list instead of notifying.
	if resp != nil && len(resp.ConfigOptions) > 0 {
		h.mu.Lock()
		state := h.sessions[sid]
		h.mu.Unlock()
		state.setOptions(resp.ConfigOptions)
	}
	return nil
}

// ListSessions asks the agent which sessions it still holds. Steve's own
// record can outlive the agent's, so this is how a stored session id is
// checked before trying to resume it.
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
	for {
		resp, err := caller.ListSessions(ctx, &acp.ListSessionsRequest{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("session/list: %w", err)
		}
		out = append(out, resp.Sessions...)
		if resp.NextCursor == nil || *resp.NextCursor == "" || len(resp.Sessions) == 0 {
			return out, nil
		}
		cursor = resp.NextCursor
	}
}

// DeleteSession asks the agent to forget a session for good, where
// CloseSession only releases it. Steve closes rather than deletes when a
// conversation ends, because a closed session can still be resumed; delete
// is for the case where Steve has dropped its own pointer and the session
// would otherwise sit in the agent's store forever with nothing able to
// reach it.
func (h *Host) DeleteSession(ctx context.Context, sid acp.SessionID) error {
	h.mu.Lock()
	if h.active[sid] != 0 {
		h.mu.Unlock()
		return ErrSessionBusy
	}
	caller, capabilities, alive := h.caller, h.capabilities, h.alive
	h.mu.Unlock()
	if !alive || caller == nil || capabilities == nil ||
		capabilities.SessionCapabilities == nil || capabilities.SessionCapabilities.Delete == nil {
		return ErrDeleteUnsupported
	}
	if _, err := caller.DeleteSession(ctx, &acp.DeleteSessionRequest{SessionID: sid}); err != nil {
		return fmt.Errorf("session/delete: %w", err)
	}
	h.mu.Lock()
	delete(h.sessions, sid)
	h.mu.Unlock()
	return nil
}
