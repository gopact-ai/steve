package acphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/acp"
)

// ErrSessionOperationUnconfirmed excludes a session whose last lifecycle
// request has no matched response. A new request is not a receipt for it.
var ErrSessionOperationUnconfirmed = errors.New("session operation outcome is unconfirmed")

type sessionOperation struct {
	method     string
	generation uint64
	process    Process
	pending    bool
	cause      error
	original   *sessionState
	state      *sessionState
}

// SessionBlockedLocked guards reuse/mutation while a lifecycle operation is
// pending or unknown. Callers must hold h.mu. It never consumes stop evidence.
func (h *Host) SessionBlockedLocked(sid acp.SessionID) error {
	op := h.sessionOperations[sid]
	if op == nil {
		return nil
	}
	if op.pending {
		return ErrSessionBusy
	}
	marker := error(ErrSessionOperationUnconfirmed)
	if op.method == "close" {
		marker = errors.Join(marker, ErrCloseUnconfirmed)
	}
	return fmt.Errorf("session/%s: %w: %w", op.method, marker, op.cause)
}
func (h *Host) beginSessionOperationLocked(ctx context.Context, sid acp.SessionID, method string) (*sessionOperation, error) {
	if err := h.SessionBlockedLocked(sid); err != nil {
		return nil, err
	}
	if (method != "configure" && h.active[sid] != 0) || h.opening[sid] != 0 {
		return nil, ErrSessionBusy
	}
	if ctx == nil {
		return nil, fmt.Errorf("session/%s: context is required", method)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("session/%s: %w", method, err)
	}
	op := &sessionOperation{method: method, generation: h.generation, process: h.proc, pending: true, original: h.sessions[sid]}
	if method == "load" || method == "resume" || method == "configure" {
		op.state = copySessionState(op.original)
	}
	if h.sessionOperations == nil {
		h.sessionOperations = map[acp.SessionID]*sessionOperation{}
	}
	h.sessionOperations[sid] = op
	return op, nil
}
func (h *Host) finishSessionOperationLocked(ctx context.Context, sid acp.SessionID, op *sessionOperation, err error) error {
	if h.sessionOperations[sid] != op {
		return fmt.Errorf("session/%s: original operation changed", op.method)
	}
	// New already has a matched creation response. A local publication failure
	// cannot retire its created-SID owner without original process-stop proof.
	if err == nil && op.method == "new" {
		if h.generation != op.generation || h.proc != op.process || h.sessions[sid] != op.original {
			err = errors.New("session/new: original session changed before publication")
		} else if !h.alive {
			err = errors.New("session/new: original process ended before publication")
		}
	}
	if err != nil {
		var response *acp.Error
		localCancel := ctx.Err() != nil && errors.Is(err, context.Cause(ctx))
		if op.method != "new" && !localCancel && errors.As(err, &response) {
			// Restore failure does not retract independent confirmations that
			// arrived before the response. Publish them into the same original
			// object only; never create a cold session or touch a replacement.
			h.preserveOperationNotificationsLocked(sid, op)
			delete(h.sessionOperations, sid)
			return fmt.Errorf("session/%s: %w", op.method, err)
		}
		op.pending = false
		op.cause = err
		return h.SessionBlockedLocked(sid)
	}
	delete(h.sessionOperations, sid)
	switch op.method {
	case "new", "load", "resume":
		if h.generation != op.generation || h.sessions[sid] != op.original {
			return fmt.Errorf("session/%s: original session changed", op.method)
		}
		if !h.alive {
			return fmt.Errorf("session/%s: original process ended", op.method)
		}
		h.sessions[sid] = op.state
	case "configure":
		if !h.alive || h.generation != op.generation || h.sessions[sid] != op.original {
			return fmt.Errorf("session/configure: original session changed")
		}
		h.preserveOperationNotificationsLocked(sid, op)
	case "close", "delete":
		if h.generation == op.generation && h.sessions[sid] == op.original {
			delete(h.sessions, sid)
		}
	}
	return nil
}
func copySessionState(original *sessionState) *sessionState {
	state := &sessionState{}
	if original == nil {
		return state
	}
	original.mu.Lock()
	defer original.mu.Unlock()
	state.options = append([]acp.SessionConfigOption(nil), original.options...)
	state.modes = append([]acp.SessionMode(nil), original.modes...)
	state.modeID = original.modeID
	state.commands = append([]acp.AvailableCommand(nil), original.commands...)
	state.optionsSequence = original.optionsSequence
	state.modeSequence = original.modeSequence
	state.modesSequence = original.modesSequence
	state.commandsSequence = original.commandsSequence
	return state
}

// Only present fields supersede prior confirmed notifications. A present empty
// list clears selectors; omission does not claim the Agent reported an empty list.
func applyOpenResponse(state *sessionState, modes *acp.SessionModeState, options *[]acp.SessionConfigOption) {
	applyOpenResponseAt(state, modes, options, 0)
}
func applyOpenResponseAt(state *sessionState, modes *acp.SessionModeState, options *[]acp.SessionConfigOption, sequence uint64) {
	// Preserve the pre-existing warm-response same-frame precedence until its
	// contract is resolved separately; clocks distinguish actual later frames.
	if options != nil {
		values := *options
		if values == nil {
			values = []acp.SessionConfigOption{}
		}
		state.setOptionsAt(values, sequence)
	}
	if modes != nil {
		state.setModesAt(modes, sequence)
	}
}
func (h *Host) closeSession(ctx context.Context, sid acp.SessionID) error {
	h.mu.Lock()
	if op := h.sessionOperations[sid]; op != nil {
		if op.pending {
			h.mu.Unlock()
			return ErrSessionBusy
		}
		// Retain the original pointer: dropping a map or stopping a replacement
		// generation cannot prove this operation's runtime has stopped.
		stopped := op.process != nil && op.process.Stopped() && h.processStoppedLocked(op.generation)
		if !stopped {
			err := h.SessionBlockedLocked(sid)
			h.mu.Unlock()
			return err
		}
		delete(h.sessionOperations, sid)
		if h.generation == op.generation && h.sessions[sid] == op.original {
			delete(h.sessions, sid)
		}
		h.mu.Unlock()
		return nil // Native retirement, not an ACP ACK or delete receipt.
	}
	if h.active[sid] != 0 || h.opening[sid] != 0 {
		h.mu.Unlock()
		return ErrSessionBusy
	}
	if h.sessions[sid] == nil {
		h.mu.Unlock()
		return nil
	}
	caller, caps, alive := h.caller, h.capabilities, h.alive
	if !alive || caller == nil || caps == nil || caps.SessionCapabilities == nil || caps.SessionCapabilities.Close == nil {
		delete(h.sessions, sid)
		h.mu.Unlock()
		return nil
	}
	op, err := h.beginSessionOperationLocked(ctx, sid, "close")
	h.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = caller.CloseSession(ctx, &acp.CloseSessionRequest{SessionID: sid})
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finishSessionOperationLocked(ctx, sid, op, err)
}
func (h *Host) deleteSession(ctx context.Context, sid acp.SessionID) error {
	h.mu.Lock()
	if err := h.SessionBlockedLocked(sid); err != nil {
		h.mu.Unlock()
		return err
	}
	if h.active[sid] != 0 || h.opening[sid] != 0 {
		h.mu.Unlock()
		return ErrSessionBusy
	}
	caller, caps, alive := h.caller, h.capabilities, h.alive
	if !alive || caller == nil || caps == nil || caps.SessionCapabilities == nil || caps.SessionCapabilities.Delete == nil {
		h.mu.Unlock()
		return ErrDeleteUnsupported
	}
	op, err := h.beginSessionOperationLocked(ctx, sid, "delete")
	h.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = caller.DeleteSession(ctx, &acp.DeleteSessionRequest{SessionID: sid})
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finishSessionOperationLocked(ctx, sid, op, err)
}

// preserveOperationNotificationsLocked keeps independent confirmations on a
// known original session after explicit rejection, without admitting a cold
// session or pretending the lifecycle request succeeded. h.mu must be held.
func (h *Host) preserveOperationNotificationsLocked(sid acp.SessionID, op *sessionOperation) {
	if op.state == nil || op.original == nil || !h.alive || h.generation != op.generation || h.sessions[sid] != op.original {
		return
	}
	copyConfirmedState(op.original, op.state)
}
func copyConfirmedState(dst, src *sessionState) {
	snapshot := copySessionState(src)
	dst.mu.Lock()
	defer dst.mu.Unlock()
	// Shadow publication must not roll back a more recent observation already
	// applied to the original object. Each semantic channel has its own clock.
	if sequenceApplies(snapshot.optionsSequence, dst.optionsSequence) {
		dst.options = snapshot.options
		dst.optionsSequence = snapshot.optionsSequence
	}
	if sequenceApplies(snapshot.modesSequence, dst.modesSequence) {
		dst.modes = snapshot.modes
		dst.modesSequence = snapshot.modesSequence
	}
	if sequenceApplies(snapshot.modeSequence, dst.modeSequence) {
		dst.modeID = snapshot.modeID
		dst.modeSequence = snapshot.modeSequence
	}
	if sequenceApplies(snapshot.commandsSequence, dst.commandsSequence) {
		dst.commands = snapshot.commands
		dst.commandsSequence = snapshot.commandsSequence
	}
}

// beginConfigOperationLocked reserves an already-open session across a config
// RPC. An existing prompt may keep running, but lifecycle changes and new
// prompts cannot enter this SID until the reservation completes. h.mu is held
// only for admission/publication, never while doing protocol I/O.
func (h *Host) beginConfigOperationLocked(ctx context.Context, sid acp.SessionID, generation uint64) (*sessionOperation, error) {
	if err := h.SessionBlockedLocked(sid); err != nil {
		return nil, err
	}
	if !h.alive || h.caller == nil || h.generation != generation || h.sessions[sid] == nil {
		return nil, fmt.Errorf("session/configure: original session is not open")
	}
	return h.beginSessionOperationLocked(ctx, sid, "configure")
}

// finishConfigOperationLocked consumes the reservation after the caller has
// validated its response. It never treats a requested value as confirmation.
// response publication ordering still needs a real SDK ingress receipt.
func (h *Host) finishConfigOperationLocked(ctx context.Context, sid acp.SessionID, op *sessionOperation, err error) error {
	if op == nil || op.method != "configure" {
		return errors.New("session/configure: invalid reservation")
	}
	return h.finishSessionOperationLocked(ctx, sid, op, err)
}

// One unbound New admission belongs to the managed-context owned-process
// model, not a promise of concurrent New admission for a future shared pool.
const (
	maxNewScratchSIDs      = 8
	maxNewScratchSIDBytes  = 256
	maxNewScratchPerSID    = 64 << 10
	maxNewScratchBytes     = 256 << 10
	maxNewScratchUpdates   = 256
	maxNewScratchOptions   = 64
	maxNewScratchGroups    = 32
	maxNewScratchChoices   = 512
	maxNewScratchCommands  = 64
	maxNewScratchModes     = 64
	maxNewScratchString    = 8 << 10
	maxNewScratchMetaNodes = 256
	maxNewScratchMetaDepth = 8
)

var errNewScratchLimit = errors.New("session/new: opening settings exceed bounded scratch limits")

type newScratchState struct {
	state *sessionState
	bytes int
}
type newOpening struct {
	caller     *acp.AgentCaller
	process    Process
	generation uint64
	pending    bool
	accepting  bool
	cause      error
	sid        acp.SessionID
	candidates map[acp.SessionID]newScratchState
	bytes      int
	updates    int
}

func (h *Host) newOpeningBlockedLocked() error {
	opening := h.newOpening
	if opening == nil {
		return nil
	}
	if opening.process != nil && opening.process.Stopped() && h.processStoppedLocked(opening.generation) {
		opening.accepting = false
		opening.candidates = nil
		if op := h.sessionOperations[opening.sid]; op != nil && op.method == "new" && !op.pending && op.process == opening.process && op.generation == opening.generation {
			delete(h.sessionOperations, opening.sid)
			if h.generation == op.generation && h.sessions[opening.sid] == op.original {
				delete(h.sessions, opening.sid)
			}
		}
		h.newOpening = nil
		return nil
	}
	if opening.pending {
		return ErrSessionBusy
	}
	return fmt.Errorf("session/new: %w: %w", ErrSessionOperationUnconfirmed, opening.cause)
}
func (h *Host) beginNewOpeningLocked(ctx context.Context) (*newOpening, error) {
	if err := h.newOpeningBlockedLocked(); err != nil {
		return nil, err
	}
	// An already-observed exited leader cannot receive a new request. Refuse
	// before dispatch rather than creating a false unknown obligation for a
	// caller racing the watcher's logical-exit publication. This is not group
	// stop proof and never retires an earlier uncertain opening.
	if h.proc == nil {
		return nil, ErrClosed
	}
	select {
	case <-h.proc.Exited():
		return nil, ErrClosed
	default:
	}
	if ctx == nil {
		return nil, errors.New("session/new: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opening := &newOpening{caller: h.caller, process: h.proc, generation: h.generation, pending: true, accepting: true, candidates: map[acp.SessionID]newScratchState{}}
	h.newOpening = opening
	return opening, nil
}
func closeNewScratch(opening *newOpening) {
	opening.accepting = false
	opening.candidates = nil
	opening.bytes = 0
}
func (h *Host) failNewOpeningLocked(ctx context.Context, opening *newOpening, err error, sid acp.SessionID, knownCreated bool) error {
	if h.newOpening != opening {
		return fmt.Errorf("session/new: original opening changed: %w", err)
	}
	closeNewScratch(opening)
	opening.pending = false
	opening.cause = err
	// An overlong returned identifier cannot itself become unbounded retained
	// scratch/debt data. Keep the original unnamed runtime obligation instead.
	if len(sid) > maxNewScratchSIDBytes {
		sid = ""
	}
	opening.sid = sid
	var response *acp.Error
	localCancel := ctx.Err() != nil && errors.Is(err, context.Cause(ctx))
	if !knownCreated && !localCancel && errors.As(err, &response) {
		h.newOpening = nil
		return fmt.Errorf("session/new: %w", err)
	}
	if knownCreated && sid != "" && h.sessions[sid] == nil && h.sessionOperations[sid] == nil {
		if h.sessionOperations == nil {
			h.sessionOperations = map[acp.SessionID]*sessionOperation{}
		}
		h.sessionOperations[sid] = &sessionOperation{method: "new", generation: opening.generation, process: opening.process, cause: err}
	}
	return fmt.Errorf("session/new: %w: %w", ErrSessionOperationUnconfirmed, err)
}
func (h *Host) stageNewSettingsLocked(sid acp.SessionID, update acp.SessionUpdate, sequence uint64) error {
	opening := h.newOpening
	if opening == nil || !opening.pending || !opening.accepting || !h.alive || opening.generation != h.generation || opening.process != h.proc || opening.caller != h.caller {
		return nil
	}
	switch update.SessionUpdate {
	case acp.SessionUpdateTypeConfigOptionUpdate, acp.SessionUpdateTypeCurrentModeUpdate, acp.SessionUpdateTypeAvailableCommandsUpdate:
	default:
		return nil
	}
	fail := func() error { closeNewScratch(opening); opening.cause = errNewScratchLimit; return errNewScratchLimit }
	if sid == "" || len(sid) > maxNewScratchSIDBytes || opening.updates >= maxNewScratchUpdates {
		return fail()
	}
	if _, exists := opening.candidates[sid]; !exists && len(opening.candidates) >= maxNewScratchSIDs {
		return fail()
	}
	if err := checkScratchFields(update.ConfigOptions, nil, update.AvailableCommands, update.CurrentModeID); err != nil {
		return fail()
	}
	candidate := opening.candidates[sid]
	next := copySessionState(candidate.state)
	// Clone only public typed values after validating bounded shape. No wire
	// parsing, SDK-private state or fabricated sequence/owner is involved.
	applySessionSettingsAt(next, cloneScratchUpdate(update), sequence)
	size, err := scratchStateBytes(next)
	if err != nil || size > maxNewScratchPerSID || opening.bytes-candidate.bytes+size > maxNewScratchBytes {
		return fail()
	}
	opening.updates++
	opening.bytes = opening.bytes - candidate.bytes + size
	opening.candidates[sid] = newScratchState{state: next, bytes: size}
	return nil
}
func (h *Host) bindNewOpeningLocked(ctx context.Context, opening *newOpening, response *acp.NewSessionResponse, receipt acp.ResponseReceipt) (*sessionOperation, error) {
	if h.newOpening != opening || !h.alive || h.generation != opening.generation || h.proc != opening.process || h.caller != opening.caller {
		if h.newOpening == opening {
			return nil, h.failNewOpeningLocked(ctx, opening, ErrClosed, "", false)
		}
		return nil, errors.New("session/new: original opening changed")
	}
	sid := response.SessionID
	if receipt.Sequence == 0 {
		return nil, h.failNewOpeningLocked(ctx, opening, errors.New("missing matched inbound receipt"), "", false)
	}
	if h.sessions[sid] != nil || h.sessionOperations[sid] != nil || h.active[sid] != 0 || h.opening[sid] != 0 {
		return nil, h.failNewOpeningLocked(ctx, opening, errors.New("matched response reused an already owned SID"), sid, true)
	}
	if opening.cause != nil {
		return nil, h.failNewOpeningLocked(ctx, opening, opening.cause, sid, true)
	}
	var options []acp.SessionConfigOption
	if response.ConfigOptions != nil {
		options = *response.ConfigOptions
	}
	if sid == "" || len(sid) > maxNewScratchSIDBytes || checkScratchFields(options, response.Modes, nil, "") != nil {
		return nil, h.failNewOpeningLocked(ctx, opening, errNewScratchLimit, sid, true)
	}
	state := copySessionState(opening.candidates[sid].state)
	// Keep the existing cold same-frame tie: legacy modes first, then options.
	state.setModesAt(response.Modes, receipt.Sequence)
	if response.ConfigOptions != nil {
		values := *response.ConfigOptions
		if values == nil {
			values = []acp.SessionConfigOption{}
		}
		state.setOptionsAt(cloneScratchOptions(values), receipt.Sequence)
	}
	if size, err := scratchStateBytes(state); err != nil || size > maxNewScratchPerSID {
		return nil, h.failNewOpeningLocked(ctx, opening, errNewScratchLimit, sid, true)
	}
	op, err := h.beginSessionOperationLocked(ctx, sid, "new")
	if err != nil {
		return nil, h.failNewOpeningLocked(ctx, opening, err, sid, true)
	}
	op.state = state
	opening.sid = sid
	closeNewScratch(opening)
	return op, nil
}
func scratchStateBytes(state *sessionState) (int, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	raw, err := json.Marshal(struct {
		Options  []acp.SessionConfigOption `json:"options"`
		Modes    []acp.SessionMode         `json:"modes"`
		Mode     acp.SessionModeID         `json:"mode"`
		Commands []acp.AvailableCommand    `json:"commands"`
	}{state.options, state.modes, state.modeID, state.commands})
	return len(raw), err
}
func checkScratchFields(options []acp.SessionConfigOption, modes *acp.SessionModeState, commands []acp.AvailableCommand, mode acp.SessionModeID) error {
	if len(options) > maxNewScratchOptions || len(commands) > maxNewScratchCommands || len(mode) > maxNewScratchString {
		return errNewScratchLimit
	}
	nodes := 0
	groups, choices := 0, 0
	text := func(values ...string) bool {
		for _, value := range values {
			if len(value) > maxNewScratchString {
				return false
			}
		}
		return true
	}
	meta := func(value acp.Meta) bool { return checkScratchMeta(value, 0, &nodes) }
	choice := func(value acp.SessionConfigSelectOption) bool {
		choices++
		if choices > maxNewScratchChoices || !text(string(value.Value), value.Name) || !meta(value.Meta) {
			return false
		}
		return value.Description == nil || text(*value.Description)
	}
	for _, option := range options {
		if !text(string(option.ID), option.Name) || !meta(option.Meta) {
			return errNewScratchLimit
		}
		if option.Category != nil && !text(string(*option.Category)) {
			return errNewScratchLimit
		}
		if option.Description != nil && !text(*option.Description) {
			return errNewScratchLimit
		}
		switch value := option.CurrentValue.(type) {
		case string:
			if !text(value) {
				return errNewScratchLimit
			}
		case acp.SessionConfigValueID:
			if !text(string(value)) {
				return errNewScratchLimit
			}
		case bool, nil:
		default:
			return errNewScratchLimit
		}
		if option.Options.Ungrouped != nil {
			for _, value := range *option.Options.Ungrouped {
				if !choice(value) {
					return errNewScratchLimit
				}
			}
		}
		if option.Options.Groups != nil {
			for _, group := range *option.Options.Groups {
				groups++
				if groups > maxNewScratchGroups || !text(string(group.Group), group.Name) || !meta(group.Meta) {
					return errNewScratchLimit
				}
				for _, value := range group.Options {
					if !choice(value) {
						return errNewScratchLimit
					}
				}
			}
		}
	}
	for _, command := range commands {
		if !text(command.Name, command.Description) || !meta(command.Meta) {
			return errNewScratchLimit
		}
		if command.Input != nil && (!text(command.Input.Hint) || !meta(command.Input.Meta)) {
			return errNewScratchLimit
		}
	}
	if modes != nil {
		if len(modes.AvailableModes) > maxNewScratchModes || !text(string(modes.CurrentModeID)) || !meta(modes.Meta) {
			return errNewScratchLimit
		}
		for _, value := range modes.AvailableModes {
			if !text(string(value.ID), value.Name) || !meta(value.Meta) || value.Description != nil && !text(*value.Description) {
				return errNewScratchLimit
			}
		}
	}
	return nil
}
func checkScratchMeta(value any, depth int, nodes *int) bool {
	*nodes = *nodes + 1
	if *nodes > maxNewScratchMetaNodes || depth > maxNewScratchMetaDepth {
		return false
	}
	switch typed := value.(type) {
	case nil, bool, float64, float32, int, int64, uint64, json.Number:
		return true
	case string:
		return len(typed) <= maxNewScratchString
	case acp.Meta:
		for key, item := range typed {
			if len(key) > maxNewScratchString || !checkScratchMeta(item, depth+1, nodes) {
				return false
			}
		}
		return true
	case map[string]any:
		for key, item := range typed {
			if len(key) > maxNewScratchString || !checkScratchMeta(item, depth+1, nodes) {
				return false
			}
		}
		return true
	case []any:
		for _, item := range typed {
			if !checkScratchMeta(item, depth+1, nodes) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func cloneScratchMeta(value acp.Meta) acp.Meta {
	if value == nil {
		return nil
	}
	cloned := make(acp.Meta, len(value))
	for key, item := range value {
		cloned[key] = cloneScratchValue(item)
	}
	return cloned
}
func cloneScratchValue(value any) any {
	switch typed := value.(type) {
	case acp.Meta:
		return cloneScratchMeta(typed)
	case map[string]any:
		copy := make(map[string]any, len(typed))
		for key, item := range typed {
			copy[key] = cloneScratchValue(item)
		}
		return copy
	case []any:
		copy := make([]any, len(typed))
		for i, item := range typed {
			copy[i] = cloneScratchValue(item)
		}
		return copy
	default:
		return value
	}
}
func cloneScratchOptions(options []acp.SessionConfigOption) []acp.SessionConfigOption {
	copy := append([]acp.SessionConfigOption{}, options...)
	cloneChoice := func(value acp.SessionConfigSelectOption) acp.SessionConfigSelectOption {
		value.Meta = cloneScratchMeta(value.Meta)
		return value
	}
	for i := range copy {
		copy[i].Meta = cloneScratchMeta(copy[i].Meta)
		if copy[i].Options.Ungrouped != nil {
			values := append(acp.UngroupedSessionConfigSelectOptions{}, (*copy[i].Options.Ungrouped)...)
			for j := range values {
				values[j] = cloneChoice(values[j])
			}
			copy[i].Options.Ungrouped = &values
		}
		if copy[i].Options.Groups != nil {
			groups := append(acp.GroupedSessionConfigSelectOptions{}, (*copy[i].Options.Groups)...)
			for j := range groups {
				groups[j].Meta = cloneScratchMeta(groups[j].Meta)
				groups[j].Options = append([]acp.SessionConfigSelectOption{}, groups[j].Options...)
				for k := range groups[j].Options {
					groups[j].Options[k] = cloneChoice(groups[j].Options[k])
				}
			}
			copy[i].Options.Groups = &groups
		}
	}
	return copy
}
func cloneScratchUpdate(update acp.SessionUpdate) acp.SessionUpdate {
	update.ConfigOptions = cloneScratchOptions(update.ConfigOptions)
	update.AvailableCommands = append([]acp.AvailableCommand{}, update.AvailableCommands...)
	for i := range update.AvailableCommands {
		update.AvailableCommands[i].Meta = cloneScratchMeta(update.AvailableCommands[i].Meta)
		if input := update.AvailableCommands[i].Input; input != nil {
			copy := *input
			copy.Meta = cloneScratchMeta(copy.Meta)
			update.AvailableCommands[i].Input = &copy
		}
	}
	return update
}

// Matched-SID staging continues to obey the same opening budget until formal
// publication. Receiving a SID does not turn oversized metadata into a grant.
func (h *Host) stageBoundNewSettingsLocked(sid acp.SessionID, op *sessionOperation, update acp.SessionUpdate, sequence uint64) error {
	opening := h.newOpening
	if opening == nil || !opening.pending || opening.sid != sid || opening.process != op.process || opening.generation != op.generation || opening.caller != h.caller {
		return nil
	}
	switch update.SessionUpdate {
	case acp.SessionUpdateTypeConfigOptionUpdate, acp.SessionUpdateTypeCurrentModeUpdate, acp.SessionUpdateTypeAvailableCommandsUpdate:
	default:
		return nil
	}
	if opening.cause != nil {
		return opening.cause
	}
	if opening.updates >= maxNewScratchUpdates || checkScratchFields(update.ConfigOptions, nil, update.AvailableCommands, update.CurrentModeID) != nil {
		opening.cause = errNewScratchLimit
		return errNewScratchLimit
	}
	next := copySessionState(op.state)
	applySessionSettingsAt(next, cloneScratchUpdate(update), sequence)
	if size, err := scratchStateBytes(next); err != nil || size > maxNewScratchPerSID {
		opening.cause = errNewScratchLimit
		return errNewScratchLimit
	}
	opening.updates++
	copyConfirmedState(op.state, next)
	return nil
}
