package node

import (
	"fmt"
	"slices"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func (one *ownedSession) commitLocked(next sessionRecord) error {
	if one.failure != nil {
		return one.failure
	}
	previousQuestions := one.record.State.Questions
	if one.record.State.Sequence == 0 {
		previousQuestions = nil
	}
	questions, err := normalizeSessionQuestions(previousQuestions, next.State.Questions)
	if err != nil {
		return err
	}
	next.State.Questions = questions
	// Every semantic transition includes the latest coalesced progress. The
	// receipt and its final text therefore cross the durable boundary together.
	if one.pendingProgress != nil {
		next.State.Progress = *one.pendingProgress
	}
	// Progress is presentation, not configuration authority: a coalesced
	// callback may predate an already-confirmed approval/model change.
	// Only this native generation can revise the canonical selector snapshot.
	// Before open has published a generation, initialization may hold the host
	// lock across its RPC. Cancellation must not wait for that RPC to finish.
	if one.host != nil && next.Generation != 0 {
		if settings, known := one.host.SettingsForGeneration(acp.SessionID(next.UpstreamID), next.Generation); known {
			next.State.Settings = settings
			next.State.ModelOption, next.State.ModelChoices = "", nil
			for _, byCategory := range []bool{true, false} {
				for _, option := range settings.Options {
					if byCategory && option.Category == "model" || !byCategory && option.ID == "model" {
						next.State.ModelOption = option.ID
						next.State.ModelChoices = slices.Clone(option.Choices)
						break
					}
				}
				if next.State.ModelOption != "" {
					break
				}
			}
		}
	}
	// A live turn follows the current configuration; a completed turn keeps
	// the configuration it actually reported, even if an idle selector changes.
	if previous, exists := one.record.Commands[next.CurrentCommand]; !exists || !previous.Settled {
		next.State.Progress.Settings = copySessionSettings(next.State.Settings)
	}
	if err := freezeTerminalReceipt(one.record, &next); err != nil {
		return err
	}
	one.pendingProgress = nil
	if one.progressTimer != nil {
		one.progressTimer.Stop()
		one.progressTimer = nil
	}
	next.State.Sequence = one.record.State.Sequence + 1
	store, err := one.service.recordsStore()
	if err == nil {
		err = store.save(one.record, next)
	}
	if err != nil {
		one.failure = fmt.Errorf("node session persistence failed: %w", err)
		close(one.changed)
		one.changed = make(chan struct{})
		return one.failure
	}
	one.record = liveSessionRecord(next)
	close(one.changed)
	one.changed = make(chan struct{})
	return nil
}

func (one *ownedSession) stateLocked(commandID string) nodewire.SessionState {
	state := copySessionState(one.record.State)
	if commandID == "" {
		commandID = one.record.CurrentCommand
	}
	if command, ok := one.record.Commands[commandID]; ok {
		copy := command
		copy.Activity = append([]string(nil), command.Activity...)
		state.Command = &copy
	}
	if state.Questions == nil {
		state.Questions = []nodewire.SessionQuestion{}
	}
	return state
}
func (one *ownedSession) state(commandID string) nodewire.SessionState {
	one.mu.Lock()
	defer one.mu.Unlock()
	return one.stateLocked(commandID)
}

// stateForRequest reads at most one command and its questions. It may observe
// an earlier binding without rebinding the still-live native conversation.
func (one *ownedSession) stateForRequest(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	one.mu.Lock()
	defer one.mu.Unlock()
	if err := one.admitLocked(req); err != nil {
		return nodewire.SessionState{}, err
	}
	store, err := one.service.recordsStore()
	if err != nil {
		return nodewire.SessionState{}, err
	}
	record, exists, err := store.read(one.record.State.ID, req.CommandID)
	if err != nil {
		return nodewire.SessionState{}, fmt.Errorf("read node session receipt: %w", err)
	}
	if !exists {
		return nodewire.SessionState{}, sessionError("unavailable", "node session receipt cannot be read")
	}
	if err := checkSessionReceipt(req, record); err != nil {
		return nodewire.SessionState{}, err
	}
	state := (&ownedSession{record: record}).stateLocked(req.CommandID)
	if req.Action == nodewire.SessionActionAttach && state.Command == nil && sessionNameValid(req.CommandID) &&
		req.InputSequence == 0 && req.Binding == record.State.Binding && one.host != nil &&
		state.State == nodewire.SessionIdle && record.BindingInputStart == state.InputAccepted {
		state.NextInputSequence = state.InputAccepted + 1
	}
	return state, nil
}

func checkSessionReceipt(req nodewire.SessionRequest, record sessionRecord) error {
	if command, exists := record.Commands[req.CommandID]; exists {
		if req.InputSequence != 0 && req.InputSequence != command.InputSequence {
			return sessionError("conflict", "receipt input sequence differs")
		}
		return nil
	}
	if req.Action == nodewire.SessionActionOpen || req.CommandID == "" {
		return nil
	}
	if req.InputSequence > 0 && req.InputSequence <= record.State.InputAccepted ||
		req.InputSequence == 0 && record.State.InputAccepted > record.BindingInputStart {
		return sessionError("receipt_expired", "input was already consumed; its receipt is no longer retained")
	}
	return nil
}

func (s *SessionService) load() error {
	store, err := s.recordsStore()
	if err != nil {
		return err
	}
	for after := ""; ; {
		ids, err := store.ids(after)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err := s.loadRecord(store, id); err != nil {
				return err
			}
		}
		after = ids[len(ids)-1]
	}
	return nil
}

func (s *SessionService) loadRecord(store *sessionRecords, id string) error {
	record, _, err := store.read(id, "")
	if err != nil {
		return err
	}
	if record.Format != 1 || record.State.ID != id || record.State.Binding.NodeID != s.server.conf().Name || record.Commands == nil || record.CommandHashes == nil {
		return sessionError("unavailable", "node session state identity differs")
	}
	one := &ownedSession{service: s, record: record, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
	if record.State.State != nodewire.SessionClosed {
		next := one.copyLocked()
		next.State.State = nodewire.SessionInterrupted
		for id, command := range next.Commands {
			if command.State.Active() {
				command.State = nodewire.SessionCommandUncertain
				command.Error = "node service restarted without a live ACP callback"
				command.Settled = false
				next.Commands[id] = command
			}
		}
		for i := range next.State.Questions {
			if next.State.Questions[i].State == nodewire.SessionQuestionPending {
				next.State.Questions[i].State = nodewire.SessionQuestionInterrupted
			}
		}
		if err := one.commitLocked(next); err != nil {
			return err
		}
	}
	// After node restart no old native callback is resumable. Keep its
	// receipt on disk and load it on demand rather than consuming a live slot.
	if !record.State.ProcessStopped {
		s.unverifiedProcesses = true
	}
	if err := s.endStoppedRuntime(record); err != nil {
		return err
	}

	return nil
}

func (s *SessionService) readRecord(id string) (sessionRecord, bool, error) {
	if !sessionIDValid(id) {
		return sessionRecord{}, false, sessionError("invalid", "invalid session record identity")
	}
	store, err := s.recordsStore()
	if err != nil {
		return sessionRecord{}, false, err
	}
	record, exists, err := store.read(id, "")
	if err != nil || !exists {
		return record, exists, err
	}
	if record.Format != 1 || record.State.ID != id || record.State.Binding.NodeID != s.server.conf().Name {
		return sessionRecord{}, false, sessionError("unavailable", "archived session identity differs")
	}
	return record, true, nil
}

// Closed records remain durable but do not consume active session slots or
// retain a Host in memory. Their original command receipts are never replayed.
func (s *SessionService) closedState(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	store, err := s.recordsStore()
	if err != nil {
		return nodewire.SessionState{}, err
	}
	record, exists, err := store.read(req.ID, req.CommandID)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if !exists || (record.State.State != nodewire.SessionClosed && record.State.State != nodewire.SessionInterrupted) {
		return nodewire.SessionState{}, sessionError("unavailable", "unknown node-owned session; reconcile the original execution")
	}
	if record.ClusterID != req.Authority.ClusterID || record.State.Binding != req.Binding {
		return nodewire.SessionState{}, sessionError("forbidden", "archived session belongs to another execution")
	}
	if err := checkSessionReceipt(req, record); err != nil {
		return nodewire.SessionState{}, err
	}
	state := record.State
	id := req.CommandID
	if id == "" {
		id = record.CurrentCommand
	}
	if command, ok := record.Commands[id]; ok {
		state.Command = &command
	}
	switch req.Action {
	case nodewire.SessionActionOpen, nodewire.SessionActionAttach, nodewire.SessionActionPoll, nodewire.SessionActionSettings:
		return state, nil
	case nodewire.SessionActionClose, nodewire.SessionActionCancel, nodewire.SessionActionAbort:
		if !state.ProcessStopped {
			return state, sessionError("uncertain", "native process stop is not confirmed")
		}
		return state, nil
	case nodewire.SessionActionPrompt:
		hash := sessionHash(struct {
			Binding  nodewire.SessionBinding
			Sequence uint64
			Text     string
			Media    []nodewire.SessionMedia
		}{req.Binding, req.InputSequence, req.Text, req.Media})
		if record.CommandHashes[req.CommandID] != hash {
			return nodewire.SessionState{}, sessionError("conflict", "closed native session cannot accept another input")
		}
		return state, nil
	case nodewire.SessionActionAnswer:
		for _, q := range state.Questions {
			if q.ID == req.QuestionID && q.Answer != nil && req.Answer != nil && *q.Answer == *req.Answer {
				return state, nil
			}
		}
		return nodewire.SessionState{}, sessionError("conflict", "closed native session has no pending question")
	default:
		return nodewire.SessionState{}, sessionError("closed", "native session is closed")
	}
}
