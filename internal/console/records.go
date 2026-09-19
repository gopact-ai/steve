package console

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

const (
	consoleStoreKind    = "console-store"
	consoleHeadKind     = "console-head"
	consoleReplyKind    = "console-reply"
	consoleExchangeKind = "console-exchange"
	consoleQuestionKind = "console-question"
)

// DurableExchange contains no process contexts, waiters or running callbacks.
// Recovery receipts are authority and survive independently of reply pruning.
type DurableExchange struct {
	Exchange
	PayloadHash          string              `json:"payload_hash,omitempty"`
	QuoteAliases         map[string]string   `json:"quote_aliases,omitempty"`
	Receipt              *consoleapi.Reply   `json:"receipt,omitempty"`
	RecoveryStopTarget   *recoveryStopTarget `json:"recovery_stop_target,omitempty"`
	RecoveryStop         *consoleapi.Reply   `json:"recovery_stop,omitempty"`
	RecoveryStopPending  string              `json:"recovery_stop_pending,omitempty"`
	RecoveryPending      bool                `json:"recovery_pending,omitempty"`
	ContinuationRejected bool                `json:"continuation_rejected,omitempty"`
}

// DurableState is an owner snapshot for stopped-service maintenance. Loading
// or storing it does not resume work or reinterpret running exchanges.
type DurableState struct {
	Replies   map[string][]consoleapi.Reply         `json:"replies"`
	Meta      map[string]Meta                       `json:"meta,omitempty"`
	Exchanges map[string][]DurableExchange          `json:"exchanges,omitempty"`
	Questions map[string]consoleapi.PendingQuestion `json:"questions,omitempty"`
}

func durableExchange(e *queuedExchange) DurableExchange {
	return DurableExchange{Exchange: e.Exchange, PayloadHash: e.PayloadHash, QuoteAliases: e.QuoteAliases,
		Receipt: e.Receipt, RecoveryStopTarget: e.RecoveryStopTarget, RecoveryStop: e.RecoveryStop,
		RecoveryStopPending: e.RecoveryStopPending, RecoveryPending: e.RecoveryPending, ContinuationRejected: e.ContinuationRejected}
}

func (e DurableExchange) queued() *queuedExchange {
	return &queuedExchange{Exchange: e.Exchange, PayloadHash: e.PayloadHash, QuoteAliases: e.QuoteAliases,
		Receipt: e.Receipt, RecoveryStopTarget: e.RecoveryStopTarget, RecoveryStop: e.RecoveryStop,
		RecoveryStopPending: e.RecoveryStopPending, RecoveryPending: e.RecoveryPending, ContinuationRejected: e.ContinuationRejected}
}

func (d DurableState) transcript() transcript {
	t := transcript{Replies: d.Replies, Meta: d.Meta, Questions: d.Questions, Exchanges: map[string][]*queuedExchange{}}
	for id, list := range d.Exchanges {
		t.Exchanges[id] = []*queuedExchange{}
		for _, e := range list {
			t.Exchanges[id] = append(t.Exchanges[id], e.queued())
		}
	}
	return t
}

type consoleControl struct {
	Revision uint64 `json:"revision"`
}

// Head and linked records preserve exact queue/reply order without storing a
// growing ID array or renumbering historical rows when a continuation is added.
type consoleHead struct {
	Replies       bool   `json:"replies,omitempty"`
	Exchanges     bool   `json:"exchanges,omitempty"`
	FirstReply    string `json:"first_reply,omitempty"`
	FirstExchange string `json:"first_exchange,omitempty"`
	Meta          *Meta  `json:"meta,omitempty"`
}
type consoleReplyRecord struct {
	Next string `json:"next,omitempty"`
	consoleapi.Reply
}
type consoleExchangeRecord struct {
	Next string `json:"next,omitempty"`
	DurableExchange
}
type consoleRecordKey struct{ kind, id string }
type consoleRecords struct {
	revision uint64
	values   map[consoleRecordKey]any
}
type consoleChange struct {
	key   consoleRecordKey
	value any
	raw   json.RawMessage
}

func decodeConsoleRecord(kind string, raw []byte) (any, error) {
	var value any
	switch kind {
	case consoleHeadKind:
		value = &consoleHead{}
	case consoleReplyKind:
		value = &consoleReplyRecord{}
	case consoleExchangeKind:
		value = &consoleExchangeRecord{}
	case consoleQuestionKind:
		value = &consoleapi.PendingQuestion{}
	default:
		return nil, fmt.Errorf("console: unknown record kind %s", kind)
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("console: record must be an object")
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return nil, fmt.Errorf("console: decode %s: %w", kind, err)
	}
	return reflect.ValueOf(value).Elem().Interface(), nil
}

func readConsoleRecords(rows *sql.Rows) (consoleRecords, error) {
	defer rows.Close()
	out := consoleRecords{values: map[consoleRecordKey]any{}}
	controlFound := false
	for rows.Next() {
		var kind, id string
		var raw []byte
		if err := rows.Scan(&kind, &id, &raw); err != nil {
			return out, err
		}
		if kind == consoleStoreKind {
			var control consoleControl
			if err := json.Unmarshal(raw, &control); err != nil || id != "state" || control.Revision == 0 {
				return out, errors.New("console: invalid store control")
			}
			out.revision, controlFound = control.Revision, true
			continue
		}
		value, err := decodeConsoleRecord(kind, raw)
		if err != nil {
			return out, err
		}
		out.values[consoleRecordKey{kind, id}] = value
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if !controlFound && len(out.values) > 0 {
		return out, errors.New("console: records exist without store control")
	}
	return out, nil
}

const consoleRecordsSQL = `SELECT kind,id,data FROM bindings WHERE kind IN ('console-store','console-head','console-reply','console-exchange','console-question')`

func loadConsoleRecords(book *ledger.Ledger) (consoleRecords, error) {
	// One statement gives all record kinds one committed read boundary.
	rows, err := book.DB().Query(consoleRecordsSQL)
	if err != nil {
		return consoleRecords{}, err
	}
	return readConsoleRecords(rows)
}

func loadConsoleRecordsTx(tx *ledger.Tx) (consoleRecords, error) {
	out := consoleRecords{values: map[consoleRecordKey]any{}}
	for _, kind := range []string{consoleStoreKind, consoleHeadKind, consoleReplyKind, consoleExchangeKind, consoleQuestionKind} {
		rows, err := tx.Bindings(kind)
		if err != nil {
			return out, err
		}
		for id, raw := range rows {
			if kind == consoleStoreKind {
				var control consoleControl
				if err := json.Unmarshal(raw, &control); err != nil || id != "state" || control.Revision == 0 {
					return out, errors.New("console: invalid store control")
				}
				out.revision = control.Revision
				continue
			}
			value, err := decodeConsoleRecord(kind, raw)
			if err != nil {
				return out, err
			}
			out.values[consoleRecordKey{kind, id}] = value
		}
	}
	if out.revision == 0 && len(out.values) > 0 {
		return out, errors.New("console: records exist without store control")
	}
	return out, nil
}

func (r consoleRecords) state() (DurableState, error) {
	out := DurableState{Replies: map[string][]consoleapi.Reply{}, Meta: map[string]Meta{}, Exchanges: map[string][]DurableExchange{}, Questions: map[string]consoleapi.PendingQuestion{}}
	visited := map[consoleRecordKey]bool{}
	values := r.values
	for key, value := range values {
		if key.id == "" {
			return out, errors.New("console: empty record identity")
		}
		if key.kind != consoleHeadKind {
			continue
		}
		head := value.(consoleHead)
		if (!head.Replies && head.FirstReply != "") || (!head.Exchanges && head.FirstExchange != "") {
			return out, fmt.Errorf("console: invalid head %s", key.id)
		}
		if head.Meta != nil {
			out.Meta[key.id] = *head.Meta
		}
		if head.Replies {
			out.Replies[key.id] = []consoleapi.Reply{}
		}
		for id := head.FirstReply; id != ""; {
			rowKey := consoleRecordKey{consoleReplyKind, id}
			row, ok := values[rowKey].(consoleReplyRecord)
			if !ok || visited[rowKey] || row.ID != id || row.Conversation != key.id {
				return out, fmt.Errorf("console: broken reply chain %s/%s", key.id, id)
			}
			visited[rowKey] = true
			out.Replies[key.id] = append(out.Replies[key.id], row.Reply)
			id = row.Next
		}
		if head.Exchanges {
			out.Exchanges[key.id] = []DurableExchange{}
		}
		keys := map[string]bool{}
		for id := head.FirstExchange; id != ""; {
			rowKey := consoleRecordKey{consoleExchangeKind, id}
			row, ok := values[rowKey].(consoleExchangeRecord)
			if !ok || visited[rowKey] || row.ID != id || row.Conversation != key.id || (row.Key != "" && keys[row.Key]) {
				return out, fmt.Errorf("console: broken exchange chain %s/%s", key.id, id)
			}
			visited[rowKey], keys[row.Key] = true, true
			out.Exchanges[key.id] = append(out.Exchanges[key.id], row.DurableExchange)
			id = row.Next
		}
	}
	for key, value := range values {
		switch key.kind {
		case consoleReplyKind, consoleExchangeKind:
			if !visited[key] {
				return out, fmt.Errorf("console: orphan record %s/%s", key.kind, key.id)
			}
		case consoleQuestionKind:
			q := value.(consoleapi.PendingQuestion)
			if q.ID != key.id {
				return out, fmt.Errorf("console: invalid question %s", key.id)
			}
			out.Questions[key.id] = q
		}
	}
	return out, nil
}

// LoadState reads the owner's records only. Document("console") is not an
// alternative authority and is never read, imported or migrated.
func LoadState(book *ledger.Ledger) (DurableState, error) {
	records, err := loadConsoleRecords(book)
	if err != nil {
		return DurableState{}, err
	}
	return records.state()
}

func consoleChanges(before consoleRecords, next transcript) ([]consoleChange, error) {
	seen := map[consoleRecordKey]bool{}
	var changes []consoleChange
	visit := func(kind, id string, value any) error {
		key := consoleRecordKey{kind, id}
		if id == "" || seen[key] {
			return fmt.Errorf("console: duplicate or empty %s identity %q", kind, id)
		}
		seen[key] = true
		if !reflect.DeepEqual(before.values[key], value) {
			changes = append(changes, consoleChange{key: key, value: value})
		}
		return nil
	}
	heads := map[string]consoleHead{}
	for conversation, list := range next.Replies {
		head := heads[conversation]
		head.Replies = true
		if len(list) > 0 {
			head.FirstReply = list[0].ID
		}
		heads[conversation] = head
		for i, reply := range list {
			if reply.Conversation != conversation {
				return nil, errors.New("console: reply conversation mismatch")
			}
			row := consoleReplyRecord{Reply: reply}
			if i+1 < len(list) {
				row.Next = list[i+1].ID
			}
			if err := visit(consoleReplyKind, reply.ID, row); err != nil {
				return nil, err
			}
		}
	}
	for conversation, list := range next.Exchanges {
		head := heads[conversation]
		head.Exchanges = true
		if len(list) > 0 && list[0] != nil {
			head.FirstExchange = list[0].ID
		}
		heads[conversation] = head
		keys := map[string]bool{}
		for i, e := range list {
			if e == nil || e.Conversation != conversation || (e.Key != "" && keys[e.Key]) {
				return nil, fmt.Errorf("console: invalid exchange in %s", conversation)
			}
			keys[e.Key] = true
			row := consoleExchangeRecord{DurableExchange: durableExchange(e)}
			if i+1 < len(list) && list[i+1] != nil {
				row.Next = list[i+1].ID
			}
			if err := visit(consoleExchangeKind, e.ID, row); err != nil {
				return nil, err
			}
		}
	}
	for conversation, meta := range next.Meta {
		head := heads[conversation]
		head.Meta = &meta
		heads[conversation] = head
	}
	for id, question := range next.Questions {
		if id != question.ID {
			return nil, errors.New("console: question identity mismatch")
		}
		if err := visit(consoleQuestionKind, id, question); err != nil {
			return nil, err
		}
	}
	for id, head := range heads {
		if err := visit(consoleHeadKind, id, head); err != nil {
			return nil, err
		}
	}
	for key := range before.values {
		if !seen[key] {
			changes = append(changes, consoleChange{key: key})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].key.kind != changes[j].key.kind {
			return changes[i].key.kind < changes[j].key.kind
		}
		return changes[i].key.id < changes[j].key.id
	})
	// Only changed records are serialized. An untouched transcript/receipt is
	// never marshaled again merely because one title or queue entry changed.
	for i := range changes {
		if changes[i].value == nil {
			continue
		}
		var err error
		changes[i].raw, err = json.Marshal(changes[i].value)
		if err != nil {
			return nil, err
		}
		// Freeze the comparison image before starting the transaction.
		changes[i].value, err = decodeConsoleRecord(changes[i].key.kind, changes[i].raw)
		if err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func consoleRevisionTx(tx *ledger.Tx) (uint64, error) {
	var raw []byte
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id='state'`, consoleStoreKind).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var control consoleControl
	if err := json.Unmarshal(raw, &control); err != nil || control.Revision == 0 {
		return 0, errors.New("console: invalid store control")
	}
	return control.Revision, nil
}

func writeConsoleChangesTx(tx *ledger.Tx, revision uint64, changes []consoleChange) error {
	current, err := consoleRevisionTx(tx)
	if err != nil {
		return err
	}
	if current != revision {
		return ledger.ErrConflict
	}
	if len(changes) == 0 && revision != 0 {
		return nil
	}
	if revision == ^uint64(0) {
		return errors.New("console: store revision exhausted")
	}
	for _, change := range changes {
		if change.value == nil {
			_, err = tx.Exec(`DELETE FROM bindings WHERE kind=? AND id=?`, change.key.kind, change.key.id)
		} else {
			err = tx.PutBinding(change.key.kind, change.key.id, change.raw)
		}
		if err != nil {
			return fmt.Errorf("console: write %s/%s: %w", change.key.kind, change.key.id, err)
		}
	}
	return tx.PutBinding(consoleStoreKind, "state", consoleControl{Revision: revision + 1})
}

// StoreStateTx replaces a stopped owner's complete state in the caller's
// transaction. It does not recover or dispatch exchanges. Normal service
// writes use the same delta writer, retaining the last committed revision.
func StoreStateTx(tx *ledger.Tx, state DurableState) error {
	before, err := loadConsoleRecordsTx(tx)
	if err != nil {
		return err
	}
	if _, err := before.state(); err != nil {
		return err
	}
	changes, err := consoleChanges(before, state.transcript())
	if err != nil {
		return err
	}
	return writeConsoleChangesTx(tx, before.revision, changes)
}

func (s *Service) saveRecords() error {
	changes, err := consoleChanges(s.records, transcript{Replies: s.replies, Meta: s.meta, Exchanges: s.exchanges, Questions: s.questions})
	if err != nil {
		return err
	}
	if err := s.book.Update(context.Background(), func(tx *ledger.Tx) error {
		return writeConsoleChangesTx(tx, s.records.revision, changes)
	}); err != nil {
		return err
	}
	if len(changes) > 0 || s.records.revision == 0 {
		s.records.revision++
	}
	for _, change := range changes {
		if change.value == nil {
			delete(s.records.values, change.key)
		} else {
			s.records.values[change.key] = change.value
		}
	}
	return nil
}

// PersistLedger loads one consistent record snapshot, then durably projects
// interrupted work before the caller invokes RecoverChats or Drain. A failed
// load/save installs no partial service state and never starts the handler.
func (s *Service) PersistLedger(book *ledger.Ledger) error {
	records, err := loadConsoleRecords(book)
	if err != nil {
		return fmt.Errorf("console: load records: %w", err)
	}
	saved, err := records.state()
	if err != nil {
		return err
	}
	// Keep the immutable comparison image separate from the installed state.
	for key, value := range records.values {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		records.values[key], err = decodeConsoleRecord(key.kind, raw)
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.book != nil || s.doc != nil {
		return errors.New("console: persistence is already configured")
	}
	previous := transcript{Replies: s.replies, Meta: s.meta, Exchanges: s.exchanges, Questions: s.questions}
	previousRunning := s.running
	s.installTranscript(saved.transcript())
	s.book, s.records = book, records
	if err := s.restoreQueueLocked(); err != nil {
		s.replies, s.meta, s.exchanges, s.questions = previous.Replies, previous.Meta, previous.Exchanges, previous.Questions
		s.running = previousRunning
		s.book, s.records = nil, consoleRecords{}
		return err
	}
	return nil
}
