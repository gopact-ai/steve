package acphost

import (
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
