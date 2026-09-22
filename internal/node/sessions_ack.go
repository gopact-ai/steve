package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

type NodeReceiptAuthorizer interface {
	AuthorizeNodeReceipt(context.Context, string, nodewire.SessionAuthority, nodewire.SessionReceipt) error
}

// AcknowledgeReceipt requires exact committed hub proof even for a retry after
// deletion. It never rebinds a session or authorizes execution.
func (s *SessionService) AcknowledgeReceipt(ctx context.Context, principal string, request nodewire.SessionReceiptRequest) error {
	a, receipt := request.Authority, request.Receipt
	if err := receipt.Validate(); err != nil {
		return err
	}
	if !sessionIDValid(receipt.SessionID) || receipt.Binding.NodeID != s.server.conf().Name ||
		!sessionNameValid(a.ClusterID) || !sessionNameValid(a.CoordinatorNodeID) || a.CoordinatorEpoch == 0 || a.WriterGeneration == 0 {
		return sessionError("invalid", "receipt acknowledgement requires exact node and coordinator identities")
	}
	authority, ok := s.server.conf().SessionAuthorizer.(NodeReceiptAuthorizer)
	if !ok {
		return sessionError("forbidden", "node has no receipt authority verifier")
	}
	if err := authority.AuthorizeNodeReceipt(ctx, principal, a, receipt); err != nil {
		return sessionError("forbidden", err.Error())
	}
	s.mu.Lock()
	one := s.sessions[receipt.SessionID]
	if one != nil {
		s.mu.Unlock()
		one.mu.Lock()
		defer one.mu.Unlock()
		// Prompt admission/archive hold owner before service. Follow that order
		// and recheck after waiting, rather than retaining a stale live pointer.
		s.mu.Lock()
		if s.sessions[receipt.SessionID] != one {
			s.mu.Unlock()
			return sessionError("unavailable", "receipt owner changed; retry exact acknowledgement")
		}
	}
	defer s.mu.Unlock()
	if s.closed {
		return sessionError("closed", "node session service is closed")
	}
	// A cold receipt stays under the service lock through its transaction,
	// serializing deletion with native-context reopen/handoff.
	if one != nil {
		if one.failure != nil {
			return one.failure
		}
	}
	store, err := s.recordsStore()
	if err != nil {
		return err
	}
	header, deleted, err := store.acknowledge(ctx, request)
	if err != nil || !deleted || one == nil {
		return err
	}
	// Publish only after SQLite committed. A later binding's hot input and
	// pending progress stay untouched when an older receipt is acknowledged.
	delete(one.record.Commands, receipt.CommandID)
	delete(one.record.CommandHashes, receipt.CommandID)
	if one.record.CurrentCommand == receipt.CommandID {
		one.record.CurrentCommand = ""
		one.record.State.Command = nil
		one.record.State.Progress = view.Progress{}
		one.record.State.Questions = nil
	}
	one.record.State.Sequence = header.State.Sequence
	close(one.changed)
	one.changed = make(chan struct{})
	return nil
}

func (s *sessionRecords) acknowledge(ctx context.Context, request nodewire.SessionReceiptRequest) (sessionRecord, bool, error) {
	var header sessionRecord
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return header, false, err
	}
	defer tx.Rollback()
	fail := func(err error) (sessionRecord, bool, error) { return header, false, err }
	receipt, authority := request.Receipt, request.Authority
	var sequence uint64
	var raw []byte
	if err := tx.QueryRow(`SELECT sequence,header FROM sessions WHERE id=?`, receipt.SessionID).Scan(&sequence, &raw); err != nil {
		return fail(err)
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return fail(err)
	}
	if header.Format != 1 || header.State.ID != receipt.SessionID || header.State.ContextID != receipt.ContextID ||
		header.State.Sequence != sequence || header.State.InputAccepted < receipt.InputSequence ||
		header.ClusterID != authority.ClusterID || header.Authority.CoordinatorEpoch > authority.CoordinatorEpoch ||
		header.Authority.WriterGeneration > authority.WriterGeneration ||
		header.Authority.CoordinatorEpoch == authority.CoordinatorEpoch && header.Authority.CoordinatorNodeID != authority.CoordinatorNodeID {
		return fail(errors.New("receipt acknowledgement differs from native session ownership"))
	}
	var inputSequence uint64
	var binding, progress []byte
	var settled bool
	err = tx.QueryRow(`SELECT input_sequence,binding,settled,command,progress FROM session_commands WHERE session_id=? AND id=?`,
		receipt.SessionID, receipt.CommandID).Scan(&inputSequence, &binding, &settled, &raw, &progress)
	if errors.Is(err, sql.ErrNoRows) {
		var conflicts int
		err = tx.QueryRow(`SELECT count(*) FROM session_commands WHERE session_id=? AND input_sequence=?`,
			receipt.SessionID, receipt.InputSequence).Scan(&conflicts)
		if err != nil {
			return fail(err)
		}
		if conflicts != 0 || header.CurrentCommand == receipt.CommandID {
			return fail(errors.New("acknowledged sequence belongs to another retained command"))
		}
		// Authorization already checked this exact original receipt against the
		// hub's immutable result. The monotonic consumed highwater forbids reuse;
		// a per-command tombstone here would itself grow without bound.
		return header, false, nil
	}
	if err != nil {
		return fail(err)
	}
	var original nodewire.SessionBinding
	var command nodewire.SessionCommand
	var frozen view.Progress
	if json.Unmarshal(binding, &original) != nil || json.Unmarshal(raw, &command) != nil || json.Unmarshal(progress, &frozen) != nil ||
		original != receipt.Binding || inputSequence != receipt.InputSequence || command.ID != receipt.CommandID ||
		command.Receipt != receipt || !settled {
		return fail(errors.New("receipt acknowledgement differs from original durable command"))
	}
	state := nodewire.SessionState{ID: receipt.SessionID, ContextID: receipt.ContextID, Binding: original, Command: &command, Progress: frozen}
	rows, err := tx.Query(`SELECT settled,question FROM session_questions WHERE session_id=? AND command_id=?`, receipt.SessionID, receipt.CommandID)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var question nodewire.SessionQuestion
		var settled bool
		if err = rows.Scan(&settled, &raw); err == nil {
			err = decodeSessionQuestionJSON(raw, &question)
		}
		if err == nil && !settled {
			err = errors.New("receipt acknowledgement has unsettled questions")
		}
		if err != nil {
			rows.Close()
			return fail(err)
		}
		state.Questions = append(state.Questions, question)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return fail(err)
	}
	frozenReceipt, err := nodewire.NewSessionReceipt(state)
	if err != nil {
		return fail(err)
	}
	if frozenReceipt != receipt {
		return fail(errors.New("terminal command or question evidence changed after receipt"))
	}
	if _, err := tx.Exec(`DELETE FROM session_questions WHERE session_id=? AND command_id=?`, receipt.SessionID, receipt.CommandID); err != nil {
		return fail(err)
	}
	if _, err := tx.Exec(`DELETE FROM session_commands WHERE session_id=? AND id=?`, receipt.SessionID, receipt.CommandID); err != nil {
		return fail(err)
	}
	if header.CurrentCommand == receipt.CommandID {
		header.CurrentCommand = ""
		progress, _ := json.Marshal(view.Progress{})
		if _, err := tx.Exec(`UPDATE session_progress SET progress=? WHERE session_id=?`, progress, receipt.SessionID); err != nil {
			return fail(err)
		}
	}
	header.State.Sequence++
	raw, err = sessionRecordJSON(header)
	if err != nil {
		return fail(err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET sequence=?,header=? WHERE id=?`, header.State.Sequence, raw, receipt.SessionID); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return header, true, nil
}
