package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
	_ "modernc.org/sqlite"
)

// sessionRecords is node-local execution evidence, not the hub's replicated
// ledger. Headers and live progress stay small; receipts are read by identity.
type sessionRecords struct {
	db       *sql.DB
	files    *sessionRecordFiles
	closeOne sync.Once
	closeErr error
}

const sessionRecordsVersion = 1
const sessionRecordsApplication = 0x53564e53 // SVNS, independent of the hub ledger.

func openSessionRecords(path string) (_ *sessionRecords, err error) {
	files, err := prepareSessionRecordFiles(path)
	if err != nil {
		return nil, err
	}
	store := &sessionRecords{files: files}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.close())
		}
	}()
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	if !files.created {
		// No writable connection or journal-mode change before identity/version
		// validation. An arbitrary SQLite database is not a node records store.
		if err := validateSessionRecords(uri); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", uri+"?mode=rw&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	store.db = db
	db.SetMaxOpenConns(1)
	if err := files.validateIdentity(); err != nil {
		return nil, err
	}
	if files.created {
		if err := initializeSessionRecords(db); err != nil {
			return nil, err
		}
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return nil, err
	}
	return store, nil
}

func validateSessionRecords(uri string) error {
	db, err := sql.Open("sqlite", uri+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var version, application int
	if err := db.QueryRow(`PRAGMA application_id`).Scan(&application); err != nil {
		return err
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if application != sessionRecordsApplication || version != sessionRecordsVersion {
		return fmt.Errorf("unsupported node records database identity/version %d/%d", application, version)
	}
	for _, statement := range sessionRecordsSchema() {
		name := strings.Fields(statement)[2]
		var actual string
		if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name=?`, name).Scan(&actual); err != nil {
			return fmt.Errorf("node records schema %s: %w", name, err)
		}
		if strings.Join(strings.Fields(actual), " ") != strings.Join(strings.Fields(statement), " ") {
			return fmt.Errorf("node records schema %s differs from version %d", name, version)
		}
	}
	return nil
}

func initializeSessionRecords(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := append(sessionRecordsSchema(),
		fmt.Sprintf(`PRAGMA application_id=%d`, sessionRecordsApplication),
		fmt.Sprintf(`PRAGMA user_version=%d`, sessionRecordsVersion))
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sessionRecordsSchema() []string {
	return append([]string{
		fmt.Sprintf(`CREATE TABLE sessions (
			id TEXT PRIMARY KEY, sequence INTEGER NOT NULL, header BLOB NOT NULL,
			command_count INTEGER NOT NULL DEFAULT 0 CHECK(command_count BETWEEN 0 AND 512),
			question_count INTEGER NOT NULL DEFAULT 0 CHECK(question_count BETWEEN 0 AND 256),
			retained_bytes INTEGER NOT NULL DEFAULT 0 CHECK(retained_bytes BETWEEN 0 AND %d))`, nodewire.NodeSessionMaxBytes),
		`CREATE TABLE session_progress (session_id TEXT PRIMARY KEY REFERENCES sessions(id), progress BLOB NOT NULL)`,
		`CREATE TABLE session_commands (
			session_id TEXT NOT NULL REFERENCES sessions(id), id TEXT NOT NULL,
			input_sequence INTEGER NOT NULL, binding BLOB NOT NULL, fingerprint TEXT NOT NULL,
			settled INTEGER NOT NULL, command BLOB NOT NULL, progress BLOB NOT NULL,
			PRIMARY KEY(session_id,id), UNIQUE(session_id,input_sequence))`,
		`CREATE TABLE session_questions (
			session_id TEXT NOT NULL REFERENCES sessions(id), id TEXT NOT NULL,
			command_id TEXT NOT NULL, settled INTEGER NOT NULL, question BLOB NOT NULL,
			PRIMARY KEY(session_id,id),
			FOREIGN KEY(session_id,command_id) REFERENCES session_commands(session_id,id))`,
		`CREATE INDEX session_questions_command ON session_questions(session_id,command_id)`,
		`CREATE TRIGGER session_header_bytes_insert AFTER INSERT ON sessions BEGIN
			UPDATE sessions SET retained_bytes=length(NEW.header) WHERE id=NEW.id; END`,
		`CREATE TRIGGER session_header_bytes_update AFTER UPDATE OF header ON sessions BEGIN
			UPDATE sessions SET retained_bytes=retained_bytes+length(NEW.header)-length(OLD.header) WHERE id=NEW.id; END`,
	}, sessionRecordQuotaSchema()...)
}

func (s *sessionRecords) close() error {
	s.closeOne.Do(func() {
		if s.db != nil {
			s.closeErr = s.db.Close()
		}
		s.closeErr = errors.Join(s.closeErr, s.files.close())
	})
	return s.closeErr
}

func sessionHeader(record sessionRecord) sessionRecord {
	record.Commands, record.CommandHashes = nil, nil
	record.State.Command, record.State.Questions = nil, nil
	record.State.Progress = view.Progress{}
	record.State.NextInputSequence = 0
	return record
}

func sessionRecordJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err == nil && len(raw) > nodewire.NodeSessionMaxBytes {
		err = sessionError("unavailable", "node session record exceeded its bound")
	}
	return raw, err
}

// save does not infer deletion from a hot record omitting old commands. Only
// an explicitly authorized acknowledgement may remove durable receipts.
func (s *sessionRecords) save(before, next sessionRecord) error {
	if !sessionIDValid(next.State.ID) || next.State.Sequence == 0 || next.State.Sequence <= before.State.Sequence || next.State.InputAccepted < before.State.InputAccepted {
		return errors.New("node session transition identity or sequence is invalid")
	}
	header, err := sessionRecordJSON(sessionHeader(next))
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if before.State.Sequence == 0 {
		if _, err := tx.Exec(`INSERT INTO sessions(id,sequence,header) VALUES(?,?,?)`, next.State.ID, next.State.Sequence, header); err != nil {
			return err
		}
	} else {
		if before.State.ID != next.State.ID {
			return errors.New("node session transition changed identity")
		}
		result, err := tx.Exec(`UPDATE sessions SET sequence=?,header=? WHERE id=? AND sequence=?`,
			next.State.Sequence, header, next.State.ID, before.State.Sequence)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return errors.New("node session transition lost its durable sequence")
		}
	}
	if before.State.Sequence == 0 || !reflect.DeepEqual(before.State.Progress, next.State.Progress) {
		progress, err := sessionRecordJSON(next.State.Progress)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO session_progress(session_id,progress) VALUES(?,?)
			ON CONFLICT(session_id) DO UPDATE SET progress=excluded.progress`, next.State.ID, progress); err != nil {
			return err
		}
	}
	if err := writeSessionCommands(tx, before, next); err != nil {
		return err
	}
	if err := writeSessionQuestions(tx, before, next); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sessionRecords) read(id, commandID string) (sessionRecord, bool, error) {
	var record sessionRecord
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return record, false, err
	}
	defer tx.Rollback()
	var raw []byte
	var sequence uint64
	if err := tx.QueryRow(`SELECT sequence,header FROM sessions WHERE id=?`, id).Scan(&sequence, &raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record, false, nil
		}
		return record, false, err
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, true, err
	}
	if record.State.ID != id || record.State.Sequence != sequence || record.Format != 1 || record.BindingInputStart > record.State.InputAccepted ||
		len(record.Commands) != 0 || len(record.CommandHashes) != 0 || len(record.State.Questions) != 0 || record.State.Command != nil || record.State.NextInputSequence != 0 {
		return record, true, errors.New("node session header identity differs")
	}
	if err := tx.QueryRow(`SELECT progress FROM session_progress WHERE session_id=?`, id).Scan(&raw); err != nil {
		return record, true, err
	}
	if err := json.Unmarshal(raw, &record.State.Progress); err != nil {
		return record, true, err
	}
	record.Commands, record.CommandHashes = map[string]nodewire.SessionCommand{}, map[string]string{}
	selected := commandID
	if selected == "" {
		selected = record.CurrentCommand
	}
	var command nodewire.SessionCommand
	var binding, progress []byte
	var fingerprint string
	var inputSequence uint64
	err = tx.QueryRow(`SELECT input_sequence,binding,fingerprint,command,progress FROM session_commands WHERE session_id=? AND id=?`, id, selected).
		Scan(&inputSequence, &binding, &fingerprint, &raw, &progress)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return record, true, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &command); err != nil {
			return record, true, err
		}
		if command.ID != selected || command.InputSequence == 0 || command.InputSequence != inputSequence || inputSequence > record.State.InputAccepted {
			return record, true, errors.New("node command receipt identity differs")
		}
		record.Commands[selected], record.CommandHashes[selected] = command, fingerprint
		if commandID != "" {
			if err := json.Unmarshal(binding, &record.State.Binding); err != nil {
				return record, true, err
			}
			if selected != record.CurrentCommand {
				if err := json.Unmarshal(progress, &record.State.Progress); err != nil {
					return record, true, err
				}
			}
		}
	}
	rows, err := tx.Query(`SELECT question FROM session_questions WHERE session_id=? AND command_id=? ORDER BY rowid`, id, selected)
	if err != nil {
		return record, true, err
	}
	defer rows.Close()
	record.State.Questions = []nodewire.SessionQuestion{}
	for rows.Next() {
		var question nodewire.SessionQuestion
		if err := rows.Scan(&raw); err != nil {
			return record, true, err
		}
		if err := json.Unmarshal(raw, &question); err != nil {
			return record, true, fmt.Errorf("decode node question: %w", err)
		}
		if question.CommandID != selected {
			return record, true, errors.New("node question command differs")
		}
		record.State.Questions = append(record.State.Questions, question)
	}
	return record, true, rows.Err()
}

func (s *sessionRecords) ids(after string) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE id>? ORDER BY id LIMIT 128`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *sessionRecords) pending(id string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT command_count FROM sessions WHERE id=?`, id).Scan(&count)
	return count, err
}

func (s *sessionRecords) resumable(id string) error {
	var unsettled int
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM session_commands WHERE session_id=? AND settled=0) +
		(SELECT COUNT(*) FROM session_questions WHERE session_id=? AND settled=0)`, id, id).Scan(&unsettled)
	if err != nil {
		return err
	}
	if unsettled != 0 {
		return sessionError("uncertain", "original native inputs or questions require reconciliation")
	}
	return nil
}

func (s *SessionService) recordsStore() (*sessionRecords, error) {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()
	if s.records == nil && s.recordsErr == nil {
		s.records, s.recordsErr = openSessionRecords(filepath.Join(s.directory(), "sessions.db"))
	}
	return s.records, s.recordsErr
}

func (s *SessionService) closeRecords() {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()
	if s.records != nil {
		if err := s.records.close(); err != nil {
			slog.Error("node session records close failed", "error", err)
			s.recordsErr = errors.Join(sessionError("closed", "node session records are closed"), err)
			return
		}
	}
	// Never reopen storage after service shutdown.
	s.recordsErr = sessionError("closed", "node session records are closed")
}

func liveSessionRecord(record sessionRecord) sessionRecord {
	command, exists := record.Commands[record.CurrentCommand]
	hash := record.CommandHashes[record.CurrentCommand]
	record.Commands, record.CommandHashes = map[string]nodewire.SessionCommand{}, map[string]string{}
	if exists {
		record.Commands[record.CurrentCommand], record.CommandHashes[record.CurrentCommand] = command, hash
	}
	questions := make([]nodewire.SessionQuestion, 0, len(record.State.Questions))
	for _, question := range record.State.Questions {
		if question.CommandID == record.CurrentCommand {
			questions = append(questions, question)
		}
	}
	record.State.Questions = questions
	return record
}

func writeSessionCommands(tx *sql.Tx, before, next sessionRecord) error {
	for id, command := range next.Commands {
		old, exists := before.Commands[id]
		exists = exists && before.State.Sequence != 0
		if exists && reflect.DeepEqual(old, command) && before.CommandHashes[id] == next.CommandHashes[id] {
			continue
		}
		if id == "" || command.ID != id || command.InputSequence == 0 || command.InputSequence > next.State.InputAccepted {
			return errors.New("node session command identity is invalid")
		}
		raw, err := sessionRecordJSON(command)
		if err != nil {
			return err
		}
		progress, err := sessionRecordJSON(next.State.Progress)
		if err != nil {
			return err
		}
		settled := command.Settled && command.State.Settled()
		if exists {
			// Binding and input sequence were fixed at admission, before any
			// native call. Rebinding the session cannot rewrite their owner.
			if old.InputSequence != command.InputSequence || before.CommandHashes[id] != next.CommandHashes[id] {
				return errors.New("node session command admission changed")
			}
			result, err := tx.Exec(`UPDATE session_commands SET settled=?,command=?,progress=? WHERE session_id=? AND id=? AND input_sequence=?`,
				settled, raw, progress, next.State.ID, id, command.InputSequence)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				return errors.New("node session command receipt disappeared")
			}
		} else {
			binding, err := json.Marshal(next.State.Binding)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO session_commands(session_id,id,input_sequence,binding,fingerprint,settled,command,progress)
				VALUES(?,?,?,?,?,?,?,?)`, next.State.ID, id, command.InputSequence, binding, next.CommandHashes[id], settled, raw, progress); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeSessionQuestions(tx *sql.Tx, before, next sessionRecord) error {
	previousQuestions := make(map[string]nodewire.SessionQuestion, len(before.State.Questions))
	for _, question := range before.State.Questions {
		previousQuestions[question.ID] = question
	}
	for _, question := range next.State.Questions {
		if old, exists := previousQuestions[question.ID]; before.State.Sequence != 0 && exists && reflect.DeepEqual(old, question) {
			continue
		}
		if question.ID == "" || question.CommandID == "" {
			return errors.New("node session question identity is invalid")
		}
		raw, err := sessionRecordJSON(question)
		if err != nil {
			return err
		}
		settled := question.State.Settled() && (question.State != nodewire.SessionQuestionAnswered || question.Answer != nil)
		result, err := tx.Exec(`INSERT INTO session_questions(session_id,id,command_id,settled,question) VALUES(?,?,?,?,?)
			ON CONFLICT(session_id,id) DO UPDATE SET settled=excluded.settled,question=excluded.question
			WHERE session_questions.command_id=excluded.command_id`, next.State.ID, question.ID, question.CommandID, settled, raw)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return errors.New("node question owner changed")
		}
	}
	return nil
}
