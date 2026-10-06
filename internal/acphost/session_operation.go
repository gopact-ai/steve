package acphost

import (
	"context"
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
	if h.active[sid] != 0 || h.opening[sid] != 0 {
		return nil, ErrSessionBusy
	}
	if ctx == nil {
		return nil, fmt.Errorf("session/%s: context is required", method)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("session/%s: %w", method, err)
	}
	op := &sessionOperation{method: method, generation: h.generation, process: h.proc, pending: true, original: h.sessions[sid]}
	if method == "load" || method == "resume" {
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
	if err != nil {
		var response *acp.Error
		localCancel := ctx.Err() != nil && errors.Is(err, context.Cause(ctx))
		if !localCancel && errors.As(err, &response) {
			delete(h.sessionOperations, sid)
			return fmt.Errorf("session/%s: %w", op.method, err)
		}
		op.pending = false
		op.cause = err
		return h.SessionBlockedLocked(sid)
	}
	delete(h.sessionOperations, sid)
	switch op.method {
	case "load", "resume":
		if h.generation != op.generation || h.sessions[sid] != op.original {
			return fmt.Errorf("session/%s: original session changed", op.method)
		}
		if !h.alive {
			return fmt.Errorf("session/%s: original process ended", op.method)
		}
		h.sessions[sid] = op.state
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
	return state
}

// Only present fields supersede prior confirmed notifications. A present empty
// list clears selectors; omission does not claim the Agent reported an empty list.
func applyOpenResponse(state *sessionState, modes *acp.SessionModeState, options *[]acp.SessionConfigOption) {
	if options != nil {
		if len(*options) == 0 {
			state.mu.Lock()
			state.options = nil
			state.mu.Unlock()
		} else {
			state.setOptions(*options)
		}
	}
	if modes != nil {
		state.mu.Lock()
		state.modes = append([]acp.SessionMode(nil), modes.AvailableModes...)
		state.modeID = modes.CurrentModeID
		state.mu.Unlock()
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
