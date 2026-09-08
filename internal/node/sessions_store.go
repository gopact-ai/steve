package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func (one *ownedSession) copyLocked() sessionRecord {
	raw, _ := json.Marshal(one.record)
	var next sessionRecord
	_ = json.Unmarshal(raw, &next)
	return next
}

func (one *ownedSession) commitLocked(next sessionRecord) error {
	if one.failure != nil {
		return one.failure
	}
	// Every semantic transition includes the latest coalesced progress. The
	// receipt and its final text therefore cross the durable boundary together.
	if one.pendingProgress != nil {
		next.State.Progress = *one.pendingProgress
		next.State.Settings = one.pendingProgress.Settings
	}
	one.pendingProgress = nil
	if one.progressTimer != nil {
		one.progressTimer.Stop()
		one.progressTimer = nil
	}
	next.State.Sequence = one.record.State.Sequence + 1
	raw, err := json.Marshal(next)
	if err == nil && len(raw) > nodewire.NodeSessionMaxBytes {
		err = sessionError("unavailable", "node session durable state exceeded its bound")
	}
	if err == nil {
		err = (&ledger.FileDocument{Path: filepath.Join(one.service.directory(), next.State.ID+".json")}).Save(raw)
	}
	if err != nil {
		one.failure = fmt.Errorf("node session persistence failed: %w", err)
		close(one.changed)
		one.changed = make(chan struct{})
		return one.failure
	}
	one.record = next
	close(one.changed)
	one.changed = make(chan struct{})
	return nil
}

func (one *ownedSession) stateLocked(commandID string) nodewire.SessionState {
	state := one.copyLocked().State
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

func (s *SessionService) load() error {
	if err := os.MkdirAll(s.directory(), 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.directory())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !sessionIDValid(id) || entry.Name() != id+".json" || !entry.Type().IsRegular() {
			return sessionError("unavailable", "invalid node session state entry")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > nodewire.NodeSessionMaxBytes {
			return sessionError("unavailable", "node session state is too large")
		}
		raw, err := os.ReadFile(filepath.Join(s.directory(), entry.Name()))
		if err != nil {
			return err
		}
		var record sessionRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("decode node session state: %w", err)
		}
		if record.Format != 1 || record.State.ID != id || record.State.Binding.NodeID != s.server.conf().Name || record.Commands == nil || record.CommandHashes == nil {
			return sessionError("unavailable", "node session state identity differs")
		}
		one := &ownedSession{service: s, record: record, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
		if record.State.State != "closed" {
			record.State.State = "interrupted"
			for id, command := range record.Commands {
				if command.State == "running" || command.State == "accepted" {
					command.State = "uncertain"
					command.Error = "node service restarted without a live ACP callback"
					command.Settled = false
					record.Commands[id] = command
				}
			}
			for i := range record.State.Questions {
				if record.State.Questions[i].State == "pending" {
					record.State.Questions[i].State = "interrupted"
				}
			}
			if err := one.commitLocked(record); err != nil {
				return err
			}
		}
		// After node restart no old native callback is resumable. Keep its
		// receipt on disk and load it on demand rather than consuming a live slot.
		if !record.State.ProcessStopped {
			s.unverifiedProcesses = true
		}

	}
	if len(s.sessions) > 1024 {
		return sessionError("unavailable", "node session retention limit reached")
	}
	return nil
}

func (s *SessionService) readRecord(id string) (sessionRecord, bool, error) {
	if !sessionIDValid(id) {
		return sessionRecord{}, false, sessionError("invalid", "invalid session record identity")
	}
	name := filepath.Join(s.directory(), id+".json")
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return sessionRecord{}, false, nil
	}
	if err != nil {
		return sessionRecord{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > nodewire.NodeSessionMaxBytes {
		return sessionRecord{}, false, sessionError("unavailable", "invalid archived session record")
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		return sessionRecord{}, false, err
	}
	var record sessionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return sessionRecord{}, false, err
	}
	if record.Format != 1 || record.State.ID != id || record.State.Binding.NodeID != s.server.conf().Name {
		return sessionRecord{}, false, sessionError("unavailable", "archived session identity differs")
	}
	return record, true, nil
}

// Closed records remain durable but do not consume active session slots or
// retain a Host in memory. Their original command receipts are never replayed.
func (s *SessionService) closedState(req nodewire.SessionRequest) (nodewire.SessionState, error) {
	record, exists, err := s.readRecord(req.ID)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if !exists || (record.State.State != "closed" && record.State.State != "interrupted") {
		return nodewire.SessionState{}, sessionError("unavailable", "unknown node-owned session; reconcile the original execution")
	}
	if record.ClusterID != req.Authority.ClusterID || record.State.Binding != req.Binding {
		return nodewire.SessionState{}, sessionError("forbidden", "archived session belongs to another execution")
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
	case "open", "attach", "poll", "settings":
		return state, nil
	case "close", "cancel", "abort":
		if !state.ProcessStopped {
			return state, sessionError("uncertain", "native process stop is not confirmed")
		}
		return state, nil
	case "prompt":
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
	case "answer":
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
