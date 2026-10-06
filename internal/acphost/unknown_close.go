package acphost

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/acp"
)

// ErrCloseUnconfirmed means a close has no matched protocol response. The
// original process may still own the session, so another RPC is not a retry.
var ErrCloseUnconfirmed = errors.New("session close outcome is unconfirmed")

type sessionClose struct {
	generation uint64
	process    Process
	pending    bool
	cause      error
}

// SessionBlockedLocked guards operations that would reuse or mutate a session
// with a pending or unknown close. The caller must hold h.mu. It does not retire
// the close, even if the process later stopped: CloseSession explicitly consumes
// that evidence. Session and process-map resets are not settlement evidence.
func (h *Host) SessionBlockedLocked(sid acp.SessionID) error {
	close := h.sessionCloses[sid]
	if close == nil {
		return nil
	}
	if close.pending {
		return ErrSessionBusy
	}
	return fmt.Errorf("%w: %w", ErrCloseUnconfirmed, close.cause)
}

// CloseSession releases a protocol session when supported, otherwise only its
// local bookkeeping. A transport loss/cancellation leaves an unknown outcome;
// only a matched response or the original owned process's confirmed stop can
// retire it. Physical cleanup is not promoted into a protocol acknowledgement.
// This method never stops a shared host to close one of its sessions.
func (h *Host) CloseSession(ctx context.Context, sid acp.SessionID) error {
	h.mu.Lock()
	if close := h.sessionCloses[sid]; close != nil {
		if close.pending {
			h.mu.Unlock()
			return ErrSessionBusy
		}
		// Keep the original Process pointer as well as its generation. A removed
		// map entry cannot invent proof when a watcher resets per-session state.
		stopped := close.process != nil && close.process.Stopped() && h.processStoppedLocked(close.generation)
		if !stopped {
			err := h.SessionBlockedLocked(sid)
			h.mu.Unlock()
			return err
		}
		delete(h.sessionCloses, sid)
		if h.generation == close.generation {
			delete(h.sessions, sid)
		}
		h.mu.Unlock()
		return nil
	}
	if h.active[sid] != 0 || h.opening[sid] != 0 {
		h.mu.Unlock()
		return ErrSessionBusy
	}
	if h.sessions[sid] == nil {
		h.mu.Unlock()
		return nil
	}
	caller, capabilities, alive := h.caller, h.capabilities, h.alive
	if !alive || caller == nil || capabilities == nil || capabilities.SessionCapabilities == nil || capabilities.SessionCapabilities.Close == nil {
		delete(h.sessions, sid)
		h.mu.Unlock()
		return nil
	}
	if ctx == nil {
		h.mu.Unlock()
		return errors.New("session/close: context is required")
	}
	if err := ctx.Err(); err != nil {
		h.mu.Unlock()
		return fmt.Errorf("session/close: %w", err)
	}
	close := &sessionClose{generation: h.generation, process: h.proc, pending: true}
	if h.sessionCloses == nil {
		h.sessionCloses = map[acp.SessionID]*sessionClose{}
	}
	h.sessionCloses[sid] = close
	h.mu.Unlock()

	_, err := caller.CloseSession(ctx, &acp.CloseSessionRequest{SessionID: sid})
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		var response *acp.Error
		// A local CancelCause can itself carry *acp.Error. Its type alone
		// is not a matched peer response and must not enable another RPC.
		localCancel := ctx.Err() != nil && errors.Is(err, context.Cause(ctx))
		if !localCancel && errors.As(err, &response) {
			// The peer explicitly rejected the request. It is safe for the owner to
			// choose another close attempt; keep the session but release this gate.
			delete(h.sessionCloses, sid)
			return fmt.Errorf("session/close: %w", err)
		}
		// Without a peer response the SDK reports transport/timeout/local cancel,
		// not whether the peer already acted. Retain the exact original identity.
		close.pending = false
		close.cause = err
		return fmt.Errorf("session/close: %w", h.SessionBlockedLocked(sid))
	}
	delete(h.sessionCloses, sid)
	if h.generation == close.generation {
		delete(h.sessions, sid)
	}
	return nil
}
