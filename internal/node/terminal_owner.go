package node

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/procgroup"
)

// nodeTerminalOwner keeps cleanup ownership inside the original native record.
// Its service context permits preparation and cleanup, never payload execution.
type nodeTerminalOwner struct{ one *ownedSession }

var _ acphost.TerminalOwner = nodeTerminalOwner{}

func (o nodeTerminalOwner) liveLocked(ctx context.Context, intent acphost.TerminalIntent) error {
	one := o.one
	if err := ctx.Err(); err != nil {
		return err
	}
	if one.failure != nil {
		return one.failure
	}
	if !one.terminalAdmission || one.service.ctx.Err() != nil ||
		string(intent.SessionID) != one.record.UpstreamID || intent.Generation != one.record.Generation ||
		!validTerminalIdentity(intent.ID) || one.record.State.State != nodewire.SessionRunning ||
		one.record.State.ProcessStopped || !one.runningLocked() || !validTerminalProcess(one.record.Process) {
		return sessionError("unavailable", "terminal has no original active native owner")
	}
	command := one.record.Commands[one.record.CurrentCommand]
	if command.InputSequence == 0 || command.InputSequence != one.record.State.InputAccepted {
		return sessionError("conflict", "terminal input differs from its original execution")
	}
	return nil
}

func (o nodeTerminalOwner) originalLocked(intent acphost.TerminalIntent) (sessionTerminal, error) {
	owner, exists := o.one.record.Terminals[intent.ID]
	if !exists || owner.UpstreamID != string(intent.SessionID) || owner.Generation != intent.Generation {
		return sessionTerminal{}, sessionError("conflict", "terminal cleanup identity differs")
	}
	return owner, nil
}

func (o nodeTerminalOwner) Reserve(ctx context.Context, intent acphost.TerminalIntent) error {
	one := o.one
	one.mu.Lock()
	defer one.mu.Unlock()
	if err := o.liveLocked(ctx, intent); err != nil {
		return err
	}
	if _, exists := one.record.Terminals[intent.ID]; exists {
		return sessionError("conflict", "terminal identity is already reserved")
	}
	next := one.copyLocked()
	if next.Terminals == nil {
		next.Terminals = map[string]sessionTerminal{}
	}
	// Released, positively stopped slots no longer own a writer. Keep all
	// outstanding slots; repeated create/release does not exhaust retention.
	var pruned []string
	for id, owner := range next.Terminals {
		if owner.Phase == "stopped" {
			delete(next.Terminals, id)
			pruned = append(pruned, id)
		}
	}
	if len(next.Terminals) >= 32 {
		return sessionError("unavailable", "terminal cleanup retention limit reached")
	}
	command := next.Commands[next.CurrentCommand]
	next.Format = 2
	next.Terminals[intent.ID] = sessionTerminal{
		ID: intent.ID, UpstreamID: next.UpstreamID, Generation: next.Generation,
		CommandID: next.CurrentCommand, InputSequence: command.InputSequence,
		Binding: next.State.Binding, Authority: next.Authority, Phase: "reserved",
	}
	if err := one.commitLocked(next); err != nil {
		return err
	}
	for _, id := range pruned {
		delete(one.terminalResults, id)
	}
	return nil
}

func (o nodeTerminalOwner) Prepared(ctx context.Context, intent acphost.TerminalIntent, preparation procgroup.Preparation, mark string) error {
	one := o.one
	one.mu.Lock()
	defer one.mu.Unlock()
	if err := o.liveLocked(ctx, intent); err != nil {
		return err
	}
	owner, err := o.originalLocked(intent)
	if err != nil {
		return err
	}
	if owner.Phase != "reserved" || owner.CommandID != one.record.CurrentCommand || owner.Binding != one.record.State.Binding {
		return sessionError("conflict", "terminal preparation has no original reservation")
	}
	next := one.copyLocked()
	owner.Phase = "preparing"
	owner.Preparation = terminalPreparation{PID: preparation.PID, Start: preparation.Start, ParentGroup: preparation.ParentGroup, Mark: mark}
	next.Terminals[intent.ID] = owner
	return one.commitLocked(next)
}

func (o nodeTerminalOwner) Active(ctx context.Context, intent acphost.TerminalIntent, identity procgroup.Identity, place procgroup.Place) error {
	one := o.one
	one.mu.Lock()
	defer one.mu.Unlock()
	if err := o.liveLocked(ctx, intent); err != nil {
		return err
	}
	owner, err := o.originalLocked(intent)
	if err != nil {
		return err
	}
	if owner.Phase != "preparing" || owner.CommandID != one.record.CurrentCommand || owner.Binding != one.record.State.Binding {
		return sessionError("conflict", "terminal split has no original preparation")
	}
	next := one.copyLocked()
	owner.Phase, owner.Preparation = "active", terminalPreparation{}
	owner.Process = sessionProcess{Identity: identity, Place: place}
	next.Terminals[intent.ID] = owner
	return one.commitLocked(next)
}

func (o nodeTerminalOwner) Admit(ctx context.Context, intent acphost.TerminalIntent, consume func(context.Context) error, validate func() error) error {
	if consume == nil || validate == nil {
		return errors.New("terminal admission requires its original live gate")
	}
	one := o.one
	one.mu.Lock()
	if err := o.liveLocked(ctx, intent); err != nil {
		one.mu.Unlock()
		return err
	}
	owner, err := o.originalLocked(intent)
	if err != nil {
		one.mu.Unlock()
		return err
	}
	if owner.Phase != "active" || owner.PayloadStarted || owner.CommandID != one.record.CurrentCommand || owner.Binding != one.record.State.Binding || one.terminalStarts[intent.ID] != nil {
		one.mu.Unlock()
		return sessionError("conflict", "terminal payload has no original active gate")
	}
	gate := &readyTerminalStart{
		Start:   nodewire.TerminalStart{ID: intent.ID, CommandID: owner.CommandID, InputSequence: owner.InputSequence, Generation: owner.Generation},
		Consume: consume, Validate: validate, Done: make(chan error, 1),
	}
	if one.terminalStarts == nil {
		one.terminalStarts = map[string]*readyTerminalStart{}
	}
	one.terminalStarts[intent.ID] = gate
	close(one.changed)
	one.changed = make(chan struct{})
	one.mu.Unlock()
	select {
	case err := <-gate.Done:
		return err
	case <-ctx.Done():
	case <-one.service.ctx.Done():
	}
	// Serialize cancellation with the fresh RPC. If admission already consumed
	// its gate, return that outcome rather than cancel a successful create.
	one.mu.Lock()
	defer one.mu.Unlock()
	select {
	case err := <-gate.Done:
		return err
	default:
	}
	if one.terminalStarts[intent.ID] == gate {
		delete(one.terminalStarts, intent.ID)
		close(one.changed)
		one.changed = make(chan struct{})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return one.service.ctx.Err()
}

func (o nodeTerminalOwner) Stopped(ctx context.Context, intent acphost.TerminalIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	one := o.one
	// Cleanup does not borrow an already-expired context across an owner wait.
	// Retain the slot when contention outlives this request; a fresh cleanup
	// can still reconcile the same positive native evidence later.
	if !one.mu.TryLock() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
			if one.mu.TryLock() {
				break
			}
		}
	}
	defer one.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, err := o.originalLocked(intent)
	if err != nil {
		return err
	}
	if owner.Phase == "stopped" {
		return nil
	}
	next := one.copyLocked()
	owner.Phase = "stopped"
	next.Terminals[intent.ID] = owner
	if err := one.commitLocked(next); err != nil {
		return err
	}
	if gate := one.terminalStarts[intent.ID]; gate != nil {
		delete(one.terminalStarts, intent.ID)
		finishTerminalStart(gate, sessionError("unavailable", "terminal owner stopped before payload admission"))
	}
	return nil
}
