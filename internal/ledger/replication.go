package ledger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrReplicaWriteBypass = errors.New("ledger: replicated writes must use ledger mutations")
	ErrReplicaFailed      = errors.New("ledger: application replica failed")
	ErrReplicaUnavailable = errors.New("ledger: replication must be attached before writing")
)

const replicaMarker = "replica.enabled"

// ReplicaPosition is an authorized, linearizable observation of the
// coordinator assignment and its applied application version.
type ReplicaPosition struct {
	Version          uint64
	CoordinatorEpoch uint64
}

// ReplicatedWrite is one validated atomic mutation. Consensus must compare
// both fences before applying the payload at ExpectedVersion+1.
type ReplicatedWrite struct {
	ID               string
	ExpectedVersion  uint64
	CoordinatorEpoch uint64
	Payload          []byte
}

// Replicator is the consumer-owned consensus port. Prepare must verify that
// this node coordinates the cluster and wait for its local application to
// catch up with a linearizable read. Propose must not return success until
// this ledger has applied the command. Neither method runs inside a SQLite
// transaction. An error can mean an unknown outcome; callers must reconcile
// the stored command or operation before repeating an external action.
type Replicator interface {
	Prepare(context.Context) (ReplicaPosition, error)
	Propose(context.Context, ReplicatedWrite) ([]byte, error)
}

// AttachReplication enables consensus-backed writes. Attach during startup,
// before serving requests. An attached ledger cannot return to local writes.
// Existing cluster state must be installed using RestoreReplica before an
// empty member can apply incremental mutations.
func (l *Ledger) AttachReplication(r Replicator) error {
	if r == nil {
		return errors.New("ledger: a replicator is required")
	}
	l.writerMu.Lock()
	defer l.writerMu.Unlock()
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	if err := validateReplicaSchema(l.db); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.replication != nil {
		return errors.New("ledger: replication already attached")
	}
	if l.replicaFailure != nil {
		return l.replicaFailure
	}
	if err := (&FileDocument{Path: filepath.Join(l.dir, replicaMarker)}).Save([]byte("1\n")); err != nil {
		return err
	}
	l.replicationRequired = true
	l.replication = r
	return nil
}

func (l *Ledger) replicationState() (Replicator, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.replication, l.replicaFailure
}

func (l *Ledger) writeState() (Replicator, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.replicaFailure != nil {
		return nil, l.replicaFailure
	}
	if l.replication == nil && l.replicationRequired {
		return nil, ErrReplicaUnavailable
	}
	return l.replication, nil
}

func (l *Ledger) failReplica(err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.replicaFailure == nil {
		l.replicaFailure = fmt.Errorf("%w: %w", ErrReplicaFailed, err)
	}
	return l.replicaFailure
}

// ReplicaVersion reads the version committed with the application facts.
func (l *Ledger) ReplicaVersion() (uint64, error) { return replicaVersion(l.db) }

func replicaVersion(q interface{ QueryRow(string, ...any) *sql.Row }) (uint64, error) {
	var version uint64
	err := q.QueryRow(`SELECT version FROM replica_state WHERE singleton = 1`).Scan(&version)
	return version, err
}

type sqlArgument struct {
	Name  string `json:"name,omitempty"`
	Kind  string `json:"kind"`
	Text  string `json:"text,omitempty"`
	Bytes []byte `json:"bytes,omitempty"`
}

// SQL values keep their driver types; JSON numbers would corrupt int64 IDs
// above 2^53, and untyped JSON would conflate a blob with its base64 string.
func encodeArguments(args []any) ([]sqlArgument, []any, error) {
	out := make([]sqlArgument, len(args))
	values := make([]any, len(args))
	for i, arg := range args {
		var name string
		if named, ok := arg.(sql.NamedArg); ok {
			name, arg = named.Name, named.Value
		}
		v, err := driver.DefaultParameterConverter.ConvertValue(arg)
		if err != nil {
			return nil, nil, err
		}
		a := sqlArgument{Name: name}
		switch v := v.(type) {
		case nil:
			a.Kind = "null"
		case int64:
			a.Kind, a.Text = "integer", strconv.FormatInt(v, 10)
		case float64:
			a.Kind, a.Text = "float", strconv.FormatFloat(v, 'g', -1, 64)
		case bool:
			a.Kind, a.Text = "bool", strconv.FormatBool(v)
		case string:
			a.Kind, a.Text = "text", v
		case []byte:
			if v == nil {
				a.Kind = "null"
			} else {
				a.Kind, a.Bytes = "blob", append([]byte{}, v...)
			}
		case time.Time:
			a.Kind, a.Text = "time", v.Format(time.RFC3339Nano)
		default:
			return nil, nil, fmt.Errorf("ledger: unsupported SQL argument %T", v)
		}
		out[i] = a
		values[i], err = a.value()
		if err != nil {
			return nil, nil, err
		}
	}
	return out, values, nil
}

func (a sqlArgument) value() (any, error) {
	var v any
	var err error
	switch a.Kind {
	case "null":
	case "integer":
		v, err = strconv.ParseInt(a.Text, 10, 64)
	case "float":
		v, err = strconv.ParseFloat(a.Text, 64)
	case "bool":
		v, err = strconv.ParseBool(a.Text)
	case "text":
		v = a.Text
	case "blob":
		v = append([]byte{}, a.Bytes...)
	case "time":
		v, err = time.Parse(time.RFC3339Nano, a.Text)
	default:
		err = fmt.Errorf("ledger: unknown SQL argument kind %q", a.Kind)
	}
	if a.Name != "" {
		v = sql.Named(a.Name, v)
	}
	return v, err
}

type sqlMutation struct {
	SQL  string        `json:"sql"`
	Args []sqlArgument `json:"args"`
	Rows int64         `json:"rows"`
}

type mutationBatch struct {
	Format      int           `json:"format"`
	Incarnation uint64        `json:"incarnation"`
	Statements  []sqlMutation `json:"statements"`
}

type writeTx struct {
	l           *Ledger
	ctx         context.Context
	raw         *sql.Tx
	replicator  Replicator
	position    ReplicaPosition
	incarnation uint64
	statements  []sqlMutation
	journal     bool
	done        bool
}

func (l *Ledger) beginWrite(ctx context.Context) (*writeTx, error) {
	l.writerMu.Lock()
	r, err := l.writeState()
	if err != nil {
		l.writerMu.Unlock()
		return nil, err
	}
	var position ReplicaPosition
	if r != nil {
		position, err = r.Prepare(ctx)
		if err != nil {
			l.writerMu.Unlock()
			return nil, err
		}
		if position.CoordinatorEpoch == 0 {
			l.writerMu.Unlock()
			return nil, errors.New("ledger: coordinator epoch is required")
		}
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		l.writerMu.Unlock()
		return nil, err
	}
	w := &writeTx{l: l, ctx: ctx, raw: tx, replicator: r, position: position}
	if r != nil {
		if err := tx.QueryRow(`SELECT value FROM meta WHERE key = 'incarnation'`).Scan(&w.incarnation); err != nil {
			w.Rollback()
			return nil, err
		}
		if w.incarnation != l.Incarnation() {
			w.Rollback()
			return nil, fmt.Errorf("%w: replica incarnation changed; reopen the writer", ErrStale)
		}
		version, err := replicaVersion(tx)
		if err != nil {
			w.Rollback()
			return nil, err
		}
		if version != position.Version {
			w.Rollback()
			return nil, fmt.Errorf("%w: application version changed", ErrConflict)
		}
	}
	return w, nil
}

func (w *writeTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if w.done {
		return nil, sql.ErrTxDone
	}
	var encoded []sqlArgument
	if w.replicator != nil {
		if err := validateMutation(query); err != nil {
			return nil, err
		}
		var err error
		encoded, args, err = encodeArguments(args)
		if err != nil {
			return nil, err
		}
	}
	result, err := w.raw.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if strings.Contains(strings.ToLower(query), "effect_entries") {
		w.journal = true
	}
	if w.replicator != nil {
		rows, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		w.statements = append(w.statements, sqlMutation{SQL: query, Args: encoded, Rows: rows})
	}
	return result, nil
}

// Row has database/sql's Scan behavior and can report a rejected query
// without executing it. Query methods cannot be used as a write bypass.
type Row struct {
	row *sql.Row
	err error
}

func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}
func (r *Row) Err() error {
	if r.err != nil {
		return r.err
	}
	return r.row.Err()
}

func (w *writeTx) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	if w.replicator != nil && !readOnlyQuery(query) {
		return &Row{err: ErrReplicaWriteBypass}
	}
	return &Row{row: w.raw.QueryRowContext(ctx, query, args...)}
}

func (w *writeTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if w.replicator != nil && !readOnlyQuery(query) {
		return nil, ErrReplicaWriteBypass
	}
	return w.raw.QueryContext(ctx, query, args...)
}

func (w *writeTx) Rollback() error {
	if w.done {
		return sql.ErrTxDone
	}
	w.done = true
	defer w.l.writerMu.Unlock()
	return w.raw.Rollback()
}

func (w *writeTx) Commit() error {
	if w.done {
		return sql.ErrTxDone
	}
	w.done = true
	defer w.l.writerMu.Unlock()
	if w.replicator == nil {
		if err := w.raw.Commit(); err != nil {
			return err
		}
		if w.journal {
			return w.l.syncEffectsJournal()
		}
		return nil
	}
	// The application callback needs this database connection. Release the
	// speculative transaction before waiting for consensus to apply it.
	if err := w.raw.Rollback(); err != nil {
		return err
	}
	if len(w.statements) == 0 {
		return nil
	}
	payload, err := json.Marshal(mutationBatch{Format: 1, Incarnation: w.incarnation, Statements: w.statements})
	if err != nil {
		return err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	commandID := "ledger-" + hex.EncodeToString(id[:])
	_, err = w.replicator.Propose(w.ctx, ReplicatedWrite{ID: commandID, ExpectedVersion: w.position.Version, CoordinatorEpoch: w.position.CoordinatorEpoch, Payload: payload})
	if err != nil {
		return err
	}
	var version uint64
	var fingerprint string
	if err := w.l.db.QueryRow(`SELECT version, fingerprint FROM replica_commands WHERE id = ?`, commandID).Scan(&version, &fingerprint); err != nil {
		return w.l.failReplica(fmt.Errorf("proposal acknowledged before local apply: %w", err))
	}
	hash := sha256.Sum256(payload)
	if version != w.position.Version+1 || fingerprint != hex.EncodeToString(hash[:]) {
		return w.l.failReplica(errors.New("proposal acknowledgement does not match the validated batch"))
	}
	return nil
}

func (l *Ledger) execWrite(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx, err := l.beginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ApplyReplicated atomically applies a committed batch and its durable
// replay receipt. A validation/storage error is fatal to the local replica;
// business conflicts must already have been rejected before consensus.
func (l *Ledger) ApplyReplicated(id string, version uint64, payload []byte) ([]byte, error) {
	if l.replicaWriter {
		return nil, ErrReplicaWriteBypass
	}
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	if _, err := l.replicationState(); err != nil {
		return nil, err
	}
	if err := l.requireReplication(); err != nil {
		return nil, l.failReplica(err)
	}
	if id == "" || version == 0 {
		return nil, l.failReplica(errors.New("invalid committed command identity"))
	}
	hash := sha256.Sum256(payload)
	fingerprint := hex.EncodeToString(hash[:])
	tx, err := l.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, l.failReplica(err)
	}
	defer tx.Rollback()
	var oldVersion uint64
	var oldFingerprint string
	var oldResult []byte
	err = tx.QueryRow(`SELECT version, fingerprint, result FROM replica_commands WHERE id = ?`, id).Scan(&oldVersion, &oldFingerprint, &oldResult)
	if err == nil {
		if oldVersion != version || oldFingerprint != fingerprint {
			return nil, l.failReplica(errors.New("committed command ID reused"))
		}
		if err := tx.Rollback(); err != nil {
			return nil, l.failReplica(err)
		}
		if err := l.syncEffectsJournal(); err != nil {
			return nil, l.failReplica(err)
		}
		return oldResult, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, l.failReplica(err)
	}
	current, err := replicaVersion(tx)
	if err != nil {
		return nil, l.failReplica(err)
	}
	if version != current+1 {
		return nil, l.failReplica(fmt.Errorf("application version %d follows %d", version, current))
	}
	var batch mutationBatch
	if err := json.Unmarshal(payload, &batch); err != nil {
		return nil, l.failReplica(err)
	}
	if batch.Format != 1 || len(batch.Statements) == 0 {
		return nil, l.failReplica(errors.New("invalid committed SQL batch"))
	}
	var incarnation uint64
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key = 'incarnation'`).Scan(&incarnation); err != nil {
		return nil, l.failReplica(err)
	}
	if batch.Incarnation != incarnation {
		return nil, l.failReplica(errors.New("committed SQL batch has another incarnation"))
	}
	journal := false
	for i, statement := range batch.Statements {
		if err := validateMutation(statement.SQL); err != nil {
			return nil, l.failReplica(err)
		}
		args := make([]any, len(statement.Args))
		for n, arg := range statement.Args {
			args[n], err = arg.value()
			if err != nil {
				return nil, l.failReplica(err)
			}
		}
		result, err := tx.Exec(statement.SQL, args...)
		if err != nil {
			return nil, l.failReplica(fmt.Errorf("statement %d: %w", i, err))
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != statement.Rows {
			return nil, l.failReplica(fmt.Errorf("statement %d affected %d rows, expected %d: %v", i, rows, statement.Rows, err))
		}
		journal = journal || strings.Contains(strings.ToLower(statement.SQL), "effect_entries")
	}
	result := []byte(`{}`)
	if _, err := tx.Exec(`INSERT INTO replica_commands(id, version, fingerprint, result) VALUES (?, ?, ?, ?)`, id, version, fingerprint, result); err != nil {
		return nil, l.failReplica(err)
	}
	advanced, err := tx.Exec(`UPDATE replica_state SET version = ? WHERE singleton = 1`, version)
	if err != nil {
		return nil, l.failReplica(err)
	}
	if rows, err := advanced.RowsAffected(); err != nil || rows != 1 {
		return nil, l.failReplica(errors.New("application version row is missing"))
	}
	if err := tx.Commit(); err != nil {
		return nil, l.failReplica(err)
	}
	if journal {
		if err := l.syncEffectsJournal(); err != nil {
			return nil, l.failReplica(err)
		}
	}
	return result, nil
}

// The runtime only issues parameterized DML. Reject transaction control,
// schema changes, multiple statements and ambient-time/random functions in
// replicated batches; they cannot be replayed as deterministic mutations.
func validateMutation(query string) error {
	// Runtime mutations name an unqualified, unquoted table. SQLite also
	// accepts single-quoted strings as identifiers; admitting that ambiguous
	// syntax would let a literal skip the protected-metadata checks below.
	target := mutationTarget.FindStringSubmatchIndex(query)
	if target == nil {
		return ErrReplicaWriteBypass
	}
	functionAllowed := true
	words, ok := scanSQL(query, func(word string, start int) {
		if start == target[2] {
			return
		}
		switch word {
		case "values", "conflict":
		default:
			functionAllowed = false
		}
	})
	if !ok || !functionAllowed || len(words) == 0 {
		return ErrReplicaWriteBypass
	}
	switch words[0] {
	case "insert", "update", "delete":
	default:
		return ErrReplicaWriteBypass
	}
	for i, word := range words {
		if word == "from" && !(words[0] == "delete" && i == 1) {
			return ErrReplicaWriteBypass
		}
		if strings.HasPrefix(word, "pragma_") || strings.HasPrefix(word, "sqlite_") {
			return fmt.Errorf("%w: system data is not replicated", ErrReplicaWriteBypass)
		}
		switch word {
		case "replica_state", "replica_commands", "meta", "select", "returning", "in", "join", "fail", "rollback", "random", "randomblob", "current_timestamp", "current_date", "current_time", "datetime", "julianday", "unixepoch", "strftime", "date", "time", "changes", "total_changes", "last_insert_rowid", "load_extension":
			return fmt.Errorf("%w: unsupported replicated SQL token %s", ErrReplicaWriteBypass, word)
		}
	}
	return nil
}

func (l *Ledger) requireReplication() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.replicationRequired {
		return nil
	}
	if err := (&FileDocument{Path: filepath.Join(l.dir, replicaMarker)}).Save([]byte("1\n")); err != nil {
		return err
	}
	l.replicationRequired = true
	return nil
}

var mutationTarget = regexp.MustCompile(`(?i)^\s*(?:insert(?:\s+or\s+(?:abort|ignore|replace))?\s+into|update(?:\s+or\s+(?:abort|ignore|replace))?|delete\s+from)\s+([a-z_][a-z0-9_]*)(?:\s|\(|$)`)

// Schema is local startup configuration, not an application command. The
// current ledger needs no triggers, attached databases or ambient defaults.
// Refuse those before attaching instead of replaying their hidden writes.
func validateReplicaSchema(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var seq int
		var name, path string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			rows.Close()
			return err
		}
		if name != "main" && name != "temp" {
			rows.Close()
			return fmt.Errorf("%w: attached databases are not replicated", ErrReplicaWriteBypass)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var temporary int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_temp_schema`).Scan(&temporary); err != nil {
		return err
	}
	if temporary != 0 {
		return fmt.Errorf("%w: temporary schema is not replicated", ErrReplicaWriteBypass)
	}
	rows, err = db.Query(`SELECT type, sql FROM sqlite_schema WHERE sql IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, query string
		if err := rows.Scan(&kind, &query); err != nil {
			return err
		}
		if kind == "trigger" || kind == "view" {
			return fmt.Errorf("%w: replicated ledger does not support %s schema", ErrReplicaWriteBypass, kind)
		}
		words, ok := sqlWords(query)
		if !ok {
			return fmt.Errorf("%w: unsupported replicated schema", ErrReplicaWriteBypass)
		}
		for _, word := range words {
			switch word {
			case "virtual", "default", "generated", "fail", "rollback", "random", "randomblob", "current_timestamp", "current_date", "current_time", "datetime", "julianday", "unixepoch", "strftime", "date", "time", "changes", "total_changes", "last_insert_rowid", "load_extension":
				return fmt.Errorf("%w: ambient schema expression %s", ErrReplicaWriteBypass, word)
			}
		}
	}
	return rows.Err()
}

func readOnlyQuery(query string) bool {
	words, ok := sqlWords(query)
	return ok && len(words) > 0 && words[0] == "select"
}

// sqlWords skips string literals but includes quoted identifiers. The narrow
// accepted grammar deliberately excludes comments and statement separators.
func sqlWords(query string) ([]string, bool) {
	return scanSQL(query, nil)
}

func scanSQL(query string, call func(string, int)) ([]string, bool) {
	query = strings.TrimRight(query, " \t\r\n")
	query = strings.TrimSuffix(query, ";")
	checkCall := func(word string, start, end int) {
		if call == nil {
			return
		}
		for end < len(query) && strings.ContainsRune(" \t\r\n", rune(query[end])) {
			end++
		}
		if end < len(query) && query[end] == '(' {
			call(word, start)
		}
	}
	var words []string
	for i := 0; i < len(query); {
		c := query[i]
		// Keep the supported grammar narrower than SQLite's lexer. In
		// particular, form-feed must not hide a function's opening parenthesis.
		if c < 0x20 && c != '\t' && c != '\r' && c != '\n' {
			return nil, false
		}
		if c == ';' || (i+1 < len(query) && (query[i:i+2] == "--" || query[i:i+2] == "/*")) {
			return nil, false
		}
		if c == '\'' || c == '"' || c == '`' || c == '[' {
			start := i
			end := c
			if c == '[' {
				end = ']'
			}
			i++
			var word strings.Builder
			closed := false
			for i < len(query) {
				if query[i] == end {
					if i+1 < len(query) && query[i+1] == end {
						word.WriteByte(end)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				word.WriteByte(query[i])
				i++
			}
			if !closed {
				return nil, false
			}
			if c != '\'' {
				name := strings.ToLower(word.String())
				words = append(words, name)
				checkCall(name, start, i)
			} else if call != nil {
				checkCall("", start, i)
			}
			continue
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' {
			start := i
			for i < len(query) && (query[i] >= 'a' && query[i] <= 'z' || query[i] >= 'A' && query[i] <= 'Z' || query[i] >= '0' && query[i] <= '9' || query[i] == '_') {
				i++
			}
			word := strings.ToLower(query[start:i])
			words = append(words, word)
			checkCall(word, start, i)
			continue
		}
		i++
	}
	return words, true
}
