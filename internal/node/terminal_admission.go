package node

import (
	"context"
	"errors"
	"sort"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// readyTerminalStart is an ephemeral payload gate backed by durable ownership.
// It is never reconstructed after restart or serialized as an execution grant.
type readyTerminalStart struct {
	Start    nodewire.TerminalStart
	Consume  func(context.Context) error
	Validate func() error
	Done     chan error
	finished bool // guarded by the original ownedSession.mu
}

// terminalAdmissionResult exists only while this process owns the original
// terminal. A durable consumed marker without this result is unknown, never
// a successful start or permission to reconstruct and replay its payload.
type terminalAdmissionResult struct {
	Start nodewire.TerminalStart
	Err   error
}

func (one *ownedSession) admitTerminal(ctx context.Context, principal string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	locked := true
	defer func() {
		if locked {
			one.mu.Unlock()
		}
	}()
	if err := one.admitLocked(req); err != nil {
		return nodewire.SessionState{}, err
	}
	if req.TerminalStart == nil || !validTerminalIdentity(req.TerminalStart.ID) {
		return nodewire.SessionState{}, sessionError("invalid", "terminal admission requires its original identity")
	}
	start := *req.TerminalStart
	if !one.terminalAdmission {
		return nodewire.SessionState{}, sessionError("unavailable", "terminal admission was not negotiated for this native session")
	}
	owner, exists := one.record.Terminals[start.ID]
	if !exists || owner.CommandID != req.CommandID || owner.CommandID != start.CommandID || owner.InputSequence != req.InputSequence || owner.InputSequence != start.InputSequence || owner.Generation != start.Generation || owner.Binding != req.Binding {
		return nodewire.SessionState{}, sessionError("conflict", "terminal admission differs from original execution")
	}
	if owner.PayloadStarted {
		return one.consumedTerminalResultLocked(start)
	}
	gate := one.terminalStarts[start.ID]
	if gate == nil || gate.Start != start || gate.Consume == nil || gate.Validate == nil || owner.Phase != "active" || one.record.CurrentCommand != owner.CommandID || !one.runningLocked() {
		return nodewire.SessionState{}, sessionError("unavailable", "terminal has no live original payload gate")
	}
	// The original owner lock may have waited behind a durable commit or stop.
	// Reauthorize on the same authenticated stream after that wait, without
	// holding owner/Host locks across the coordinator round-trip.
	one.mu.Unlock()
	locked = false
	err := one.service.authorize(ctx, principal, req)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if !one.mu.TryLock() {
		// Do not borrow the last challenge across another owner wait. This
		// refusal consumed no gate; a new request must obtain a fresh grant.
		return nodewire.SessionState{}, sessionError("busy", "terminal admission owner was contended after authority check")
	}
	locked = true
	if err := one.admitLocked(req); err != nil {
		return nodewire.SessionState{}, err
	}
	current, exists := one.record.Terminals[start.ID]
	if !exists || current != owner || one.terminalStarts[start.ID] != gate ||
		one.record.CurrentCommand != owner.CommandID || !one.runningLocked() {
		// A concurrent duplicate may have consumed this exact gate while the
		// challenge was in flight. It observes only the original durable state.
		if exists && current.PayloadStarted && current.ID == owner.ID &&
			current.UpstreamID == owner.UpstreamID && current.Generation == owner.Generation &&
			current.CommandID == owner.CommandID && current.InputSequence == owner.InputSequence &&
			current.Binding == owner.Binding {
			return one.consumedTerminalResultLocked(start)
		}
		return nodewire.SessionState{}, sessionError("conflict", "original terminal gate changed while checking authority")
	}
	if err := ctx.Err(); err != nil {
		return nodewire.SessionState{}, err
	}
	if err := gate.Validate(); err != nil {
		delete(one.terminalStarts, start.ID)
		finishTerminalStart(gate, err)
		return nodewire.SessionState{}, err
	}
	if err := ctx.Err(); err != nil {
		delete(one.terminalStarts, start.ID)
		finishTerminalStart(gate, err)
		return nodewire.SessionState{}, err
	}
	// Commit once-only consumption before making payload executable. A commit
	// whose outcome is uncertain leaves the gate closed and cannot be retried.
	next := one.copyLocked()
	changed := next.Terminals[start.ID]
	changed.PayloadStarted = true
	next.Terminals[start.ID] = changed
	if err := one.commitLocked(next); err != nil {
		delete(one.terminalStarts, start.ID)
		finishTerminalStart(gate, err)
		return nodewire.SessionState{}, err
	}
	if err := gate.Validate(); err != nil {
		delete(one.terminalStarts, start.ID)
		one.finishTerminalAdmissionLocked(gate, err)
		return nodewire.SessionState{}, err
	}
	if err := ctx.Err(); err != nil {
		delete(one.terminalStarts, start.ID)
		one.finishTerminalAdmissionLocked(gate, err)
		return nodewire.SessionState{}, err
	}
	if err := gate.Consume(ctx); err != nil {
		delete(one.terminalStarts, start.ID)
		err = errors.Join(sessionError("unavailable", "terminal payload start was not confirmed"), err)
		one.finishTerminalAdmissionLocked(gate, err)
		return nodewire.SessionState{}, err
	}
	delete(one.terminalStarts, start.ID)
	one.finishTerminalAdmissionLocked(gate, nil)
	return one.stateLocked(req.CommandID), nil
}

func (one *ownedSession) consumedTerminalResultLocked(start nodewire.TerminalStart) (nodewire.SessionState, error) {
	if result, known := one.terminalResults[start.ID]; known && result.Start == start {
		return one.stateLocked(start.CommandID), result.Err
	}
	return nodewire.SessionState{}, sessionError("unavailable", "original consumed terminal admission has no confirmed live outcome")
}

func (one *ownedSession) finishTerminalAdmissionLocked(gate *readyTerminalStart, err error) {
	if one.terminalResults == nil {
		one.terminalResults = map[string]terminalAdmissionResult{}
	}
	one.terminalResults[gate.Start.ID] = terminalAdmissionResult{Start: gate.Start, Err: err}
	finishTerminalStart(gate, err)
}

func finishTerminalStart(gate *readyTerminalStart, err error) {
	if gate.Done != nil && !gate.finished {
		gate.finished = true
		gate.Done <- err
		close(gate.Done)
	}
}

func (one *ownedSession) projectedTerminalStartsLocked(commandID string) []nodewire.TerminalStart {
	if !one.terminalAdmission || commandID != one.record.CurrentCommand || !one.runningLocked() {
		return nil
	}
	var out []nodewire.TerminalStart
	for id, gate := range one.terminalStarts {
		owner, exists := one.record.Terminals[id]
		if gate != nil && exists && !owner.PayloadStarted && owner.Phase == "active" && owner.CommandID == commandID && owner.Binding == one.record.State.Binding {
			out = append(out, gate.Start)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
