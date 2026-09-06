// Package ledger is the single authority for every name, binding and
// operation state in Steve.
//
// It records three kinds of thing and nothing else. A Command is an
// idempotent request: the same command id always yields the same result. An
// Operation is a long-lived transaction with a state machine, moved only by
// Transition, which checks the expected state, every lease the transition
// claims to hold, and runs the caller's mutations in the same SQLite
// transaction that appends the Event. An Event is the immutable record of
// one transition.
//
// Two files sit beside the database and deliberately do not roll back with
// it. The incarnation file is a counter the operator rotates whenever the
// database is restored from backup; every lease and receipt carries it, so a
// writer from before the restore cannot match. The effects journal records
// every action that leaves a trace outside Steve — started before the
// action, confirmed after its receipt — so a restored database can be told
// what really happened before it accepts new commands.
package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Errors callers are expected to distinguish.
var (
	// ErrStale is a lease that does not match the ledger exactly: wrong
	// incarnation, superseded epoch, another holder, or expired. Every
	// guarded write fails with it rather than proceeding.
	ErrStale = errors.New("ledger: lease is stale")
	// ErrHeld is an acquire refused because another holder's lease is live.
	ErrHeld = errors.New("ledger: resource is held")
	// ErrConflict is a compare-and-set that lost: the operation or name was
	// not in the state the caller expected.
	ErrConflict = errors.New("ledger: conflict")
	// ErrInFlight is a command that is already running and has no result yet.
	ErrInFlight = errors.New("ledger: command in flight")
	// ErrRecoveryRequired is an open against a database older than the
	// incarnation file: a restore happened and reconciliation must run
	// before the ledger accepts writes.
	ErrRecoveryRequired = errors.New("ledger: database predates the incarnation file; recover first")
	// ErrIncarnationLost is an incarnation file older than the database,
	// which cannot happen unless the file was lost or rolled back — a
	// state the ledger refuses rather than guesses at.
	ErrIncarnationLost = errors.New("ledger: incarnation file is older than the database")
)

const (
	rfc3339nano     = time.RFC3339Nano
	incarnationFile = "incarnation"
	databaseFile    = "ledger.db"
	journalFile     = "effects.log"
	schemaVersion   = 1
)

// Ledger is one open authority.
type Ledger struct {
	dir         string
	db          *sql.DB
	incarnation uint64
	journal     *Journal
	now         func() time.Time

	mu       sync.Mutex
	recovery bool

	region  string
	issuers map[string]Issuer
}

// Options tune an Open.
type Options struct {
	// Recover accepts a database older than the incarnation file, which is
	// what a restore from backup looks like. The caller is then expected to
	// reconcile from the effects journal before serving.
	Recover bool
	Now     func() time.Time
}

// Open opens or creates the ledger in dir.
func Open(dir string, opts Options) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ledger dir: %w", err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	incarnation, err := readIncarnation(filepath.Join(dir, incarnationFile))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, databaseFile)+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	// One writer at a time; SQLite serialises anyway, and a single
	// connection keeps transactions from deadlocking on the busy handler.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	seen, err := metaUint(db, "incarnation")
	if err != nil {
		db.Close()
		return nil, err
	}
	l := &Ledger{dir: dir, db: db, incarnation: incarnation, now: now}
	switch {
	case seen == incarnation:
	case seen > incarnation:
		db.Close()
		return nil, ErrIncarnationLost
	default: // seen < incarnation: the database is older than the file
		if seen != 0 && !opts.Recover {
			db.Close()
			return nil, ErrRecoveryRequired
		}
		l.recovery = seen != 0
		if err := setMetaUint(db, "incarnation", incarnation); err != nil {
			db.Close()
			return nil, err
		}
	}
	journal, err := OpenJournal(filepath.Join(dir, journalFile), incarnation, now)
	if err != nil {
		db.Close()
		return nil, err
	}
	l.journal = journal
	return l, nil
}

// Rotate bumps the incarnation. Run it after restoring the database from a
// backup, before opening the ledger for service: everything issued under the
// old incarnation — leases, generations, receipts — stops matching.
func Rotate(dir string) (uint64, error) {
	path := filepath.Join(dir, incarnationFile)
	current, err := readIncarnation(path)
	if err != nil {
		return 0, err
	}
	next := current + 1
	if err := writeIncarnation(path, next); err != nil {
		return 0, err
	}
	return next, nil
}

func (l *Ledger) Close() error {
	l.journal.Close()
	return l.db.Close()
}

// Incarnation is the value every lease and receipt must carry.
func (l *Ledger) Incarnation() uint64 { return l.incarnation }

// InRecovery reports whether this open accepted an older database and still
// owes a reconciliation pass.
func (l *Ledger) InRecovery() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recovery
}

// RecoveryDone clears the recovery flag once reconciliation has run.
func (l *Ledger) RecoveryDone() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recovery = false
}

// Journal is the effects journal beside the database.
func (l *Ledger) Journal() *Journal { return l.journal }

// DB exposes the connection for stores that keep their own tables inside the
// same file. They must not touch the ledger's tables directly.
func (l *Ledger) DB() *sql.DB { return l.db }

// ---------------------------------------------------------------- schema

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commands (
			id TEXT PRIMARY KEY, kind TEXT NOT NULL, actor TEXT NOT NULL,
			received_at TEXT NOT NULL, finished_at TEXT, result TEXT, error TEXT)`,
		`CREATE TABLE IF NOT EXISTS operations (
			id TEXT PRIMARY KEY, kind TEXT NOT NULL, state TEXT NOT NULL,
			revision INTEGER NOT NULL, incarnation INTEGER NOT NULL,
			data TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS operations_kind_state ON operations(kind, state)`,
		`CREATE TABLE IF NOT EXISTS events (
			seq INTEGER PRIMARY KEY AUTOINCREMENT, operation_id TEXT NOT NULL,
			revision INTEGER NOT NULL, incarnation INTEGER NOT NULL,
			from_state TEXT NOT NULL, to_state TEXT NOT NULL, actor TEXT NOT NULL,
			fencings TEXT NOT NULL, effects TEXT NOT NULL, at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS events_operation ON events(operation_id, seq)`,
		`CREATE TABLE IF NOT EXISTS names (
			name TEXT PRIMARY KEY, version INTEGER NOT NULL, artifact TEXT NOT NULL,
			updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS leases (
			resource_key TEXT PRIMARY KEY, incarnation INTEGER NOT NULL,
			epoch INTEGER NOT NULL, holder TEXT NOT NULL, expires_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS bindings (
			kind TEXT NOT NULL, id TEXT NOT NULL, data TEXT NOT NULL,
			updated_at TEXT NOT NULL, PRIMARY KEY (kind, id))`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("ledger schema: %w", err)
		}
	}
	version, err := metaUint(db, "schema")
	if err != nil {
		return err
	}
	if version == 0 {
		return setMetaUint(db, "schema", schemaVersion)
	}
	if version != schemaVersion {
		return fmt.Errorf("ledger schema v%d, this binary speaks v%d", version, schemaVersion)
	}
	return nil
}

func metaUint(db *sql.DB, key string) (uint64, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(value, 10, 64)
}

func setMetaUint(db *sql.DB, key string, value uint64) error {
	_, err := db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, strconv.FormatUint(value, 10))
	return err
}

func readIncarnation(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := writeIncarnation(path, 1); err != nil {
			return 0, err
		}
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read incarnation: %w", err)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("incarnation file %q is not a positive integer", path)
	}
	return value, nil
}

// writeIncarnation is durable: temp file, fsync, rename, fsync dir. Losing
// this file is worse than losing the database.
func writeIncarnation(path string, value uint64) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".incarnation-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.WriteString(strconv.FormatUint(value, 10) + "\n"); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ---------------------------------------------------------------- commands

// Command runs an idempotent request. A command id seen before returns its
// stored result without running again; one still running returns
// ErrInFlight. The result is stored whether run succeeded or failed, because
// a failure is also an answer the retrying client must get back.
func (l *Ledger) Command(ctx context.Context, id, kind, actor string, run func(ctx context.Context) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	if id == "" {
		return nil, false, errors.New("ledger: command id is required")
	}
	now := l.now().UTC().Format(time.RFC3339Nano)
	res, err := l.db.ExecContext(ctx, `INSERT OR IGNORE INTO commands(id, kind, actor, received_at) VALUES (?, ?, ?, ?)`,
		id, kind, actor, now)
	if err != nil {
		return nil, false, err
	}
	inserted, _ := res.RowsAffected()
	if inserted == 0 {
		var finished sql.NullString
		var result, cmdErr sql.NullString
		if err := l.db.QueryRowContext(ctx, `SELECT finished_at, result, error FROM commands WHERE id = ?`, id).Scan(&finished, &result, &cmdErr); err != nil {
			return nil, false, err
		}
		if !finished.Valid {
			return nil, true, ErrInFlight
		}
		if cmdErr.Valid && cmdErr.String != "" {
			return json.RawMessage(result.String), true, errors.New(cmdErr.String)
		}
		return json.RawMessage(result.String), true, nil
	}
	result, runErr := run(ctx)
	errText := ""
	if runErr != nil {
		errText = runErr.Error()
	}
	if _, err := l.db.ExecContext(ctx, `UPDATE commands SET finished_at = ?, result = ?, error = ? WHERE id = ?`,
		l.now().UTC().Format(time.RFC3339Nano), string(result), errText, id); err != nil {
		return result, false, err
	}
	return result, false, runErr
}

// ---------------------------------------------------------------- leases

// Lease is a fencing token: exactly this tuple, or nothing.
type Lease struct {
	// Region is who issued the lease; empty is the local ledger.
	Region      string    `json:"region,omitempty"`
	Key         string    `json:"key"`
	Incarnation uint64    `json:"incarnation"`
	Epoch       uint64    `json:"epoch"`
	Holder      string    `json:"holder"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Acquire takes a resource for holder. It succeeds when the resource is free
// or its current lease has expired; the epoch always moves forward, so a
// previous holder that wakes up late cannot match.
func (l *Ledger) Acquire(ctx context.Context, key, holder string, ttl time.Duration) (Lease, error) {
	if key == "" || holder == "" || ttl <= 0 {
		return Lease{}, errors.New("ledger: acquire needs key, holder and ttl")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()
	now := l.now()
	var epoch uint64
	var current string
	var expires string
	err = tx.QueryRowContext(ctx, `SELECT epoch, holder, expires_at FROM leases WHERE resource_key = ?`, key).Scan(&epoch, &current, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		epoch = 0
	case err != nil:
		return Lease{}, err
	default:
		if current != "" && current != holder {
			until, _ := time.Parse(time.RFC3339Nano, expires)
			if until.After(now) {
				return Lease{}, fmt.Errorf("%w: %s by %s until %s", ErrHeld, key, current, until.Format(time.RFC3339))
			}
		}
	}
	lease := Lease{Key: key, Incarnation: l.incarnation, Epoch: epoch + 1, Holder: holder, ExpiresAt: now.Add(ttl)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO leases(resource_key, incarnation, epoch, holder, expires_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(resource_key) DO UPDATE SET incarnation = excluded.incarnation, epoch = excluded.epoch, holder = excluded.holder, expires_at = excluded.expires_at`,
		key, lease.Incarnation, lease.Epoch, holder, lease.ExpiresAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// Renew extends a lease that still matches exactly.
func (l *Ledger) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()
	if err := l.checkLease(ctx, tx, lease); err != nil {
		return Lease{}, err
	}
	lease.ExpiresAt = l.now().Add(ttl)
	if _, err := tx.ExecContext(ctx, `UPDATE leases SET expires_at = ? WHERE resource_key = ?`,
		lease.ExpiresAt.UTC().Format(time.RFC3339Nano), lease.Key); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// Release gives a resource up. A stale lease releases nothing.
func (l *Ledger) Release(ctx context.Context, lease Lease) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := l.checkLease(ctx, tx, lease); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leases SET holder = '', expires_at = ? WHERE resource_key = ?`,
		l.now().UTC().Format(time.RFC3339Nano), lease.Key); err != nil {
		return err
	}
	return tx.Commit()
}

// Transfer hands a lease from its holder to another under a new epoch: the
// old tuple stops matching, the new holder gets a fresh one, and nobody in
// between could have taken the resource. A reservation becomes an
// attempt's lease this way.
func (l *Ledger) Transfer(ctx context.Context, lease Lease, to string, ttl time.Duration) (Lease, error) {
	if to == "" || ttl <= 0 {
		return Lease{}, errors.New("ledger: transfer needs a holder and a ttl")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()
	if err := l.checkLease(ctx, tx, lease); err != nil {
		return Lease{}, err
	}
	next := Lease{Key: lease.Key, Incarnation: l.incarnation, Epoch: lease.Epoch + 1, Holder: to, ExpiresAt: l.now().Add(ttl)}
	if _, err := tx.ExecContext(ctx, `UPDATE leases SET incarnation = ?, epoch = ?, holder = ?, expires_at = ? WHERE resource_key = ? AND epoch = ?`,
		next.Incarnation, next.Epoch, to, next.ExpiresAt.UTC().Format(time.RFC3339Nano), lease.Key, lease.Epoch); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return next, nil
}

// Invalidate forces the epoch forward and clears the holder, whoever it is.
// Supersede uses it on the resources an expired attempt still holds.
func (l *Ledger) Invalidate(ctx context.Context, key string) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO leases(resource_key, incarnation, epoch, holder, expires_at) VALUES (?, ?, 1, '', ?)
		ON CONFLICT(resource_key) DO UPDATE SET epoch = leases.epoch + 1, holder = '', expires_at = excluded.expires_at, incarnation = excluded.incarnation`,
		key, l.incarnation, l.now().UTC().Format(time.RFC3339Nano))
	return err
}

// InvalidateHeldBy forces the epoch forward on every resource holder still
// holds — and only those: a resource someone else has since acquired keeps
// its lease.
func (l *Ledger) InvalidateHeldBy(ctx context.Context, holder string) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT resource_key FROM leases WHERE holder = ?`, holder)
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	rows.Close()
	for _, key := range keys {
		if _, err := l.db.ExecContext(ctx, `UPDATE leases SET epoch = epoch + 1, holder = '', expires_at = ? WHERE resource_key = ? AND holder = ?`,
			l.now().UTC().Format(time.RFC3339Nano), key, holder); err != nil {
			return keys, err
		}
	}
	return keys, nil
}

// InvalidateAll forces every epoch forward. Recovery does this after
// rotating the incarnation: nothing issued before survives.
func (l *Ledger) InvalidateAll(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `UPDATE leases SET epoch = epoch + 1, holder = '', expires_at = ?, incarnation = ?`,
		l.now().UTC().Format(time.RFC3339Nano), l.incarnation)
	return err
}

func (l *Ledger) checkLease(ctx context.Context, tx *sql.Tx, lease Lease) error {
	var incarnation, epoch uint64
	var holder, expires string
	err := tx.QueryRowContext(ctx, `SELECT incarnation, epoch, holder, expires_at FROM leases WHERE resource_key = ?`, lease.Key).
		Scan(&incarnation, &epoch, &holder, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s was never leased", ErrStale, lease.Key)
	}
	if err != nil {
		return err
	}
	if incarnation != lease.Incarnation || epoch != lease.Epoch || holder != lease.Holder {
		return fmt.Errorf("%w: %s is at incarnation %d epoch %d holder %q", ErrStale, lease.Key, incarnation, epoch, holder)
	}
	until, _ := time.Parse(time.RFC3339Nano, expires)
	if !until.After(l.now()) {
		return fmt.Errorf("%w: %s expired at %s", ErrStale, lease.Key, until.Format(time.RFC3339))
	}
	return nil
}

// ---------------------------------------------------------------- operations

// Operation is one long-lived transaction's current state.
type Operation struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	State       string          `json:"state"`
	Revision    int64           `json:"revision"`
	Incarnation uint64          `json:"incarnation"`
	Data        json.RawMessage `json:"data"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Event is the record of one transition.
type Event struct {
	Seq         int64           `json:"seq"`
	OperationID string          `json:"operation_id"`
	Revision    int64           `json:"revision"`
	Incarnation uint64          `json:"incarnation"`
	From        string          `json:"from"`
	To          string          `json:"to"`
	Actor       string          `json:"actor"`
	Fencings    []Lease         `json:"fencings"`
	Effects     json.RawMessage `json:"effects"`
	At          time.Time       `json:"at"`
}

// Begin creates an operation in its initial state. It is itself recorded as
// an event from "" to the initial state.
func (l *Ledger) Begin(ctx context.Context, id, kind, initial, actor string, data any) (Operation, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Operation{}, err
	}
	now := l.now().UTC()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations(id, kind, state, revision, incarnation, data, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?, ?, ?)`,
		id, kind, initial, l.incarnation, string(raw), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Operation{}, fmt.Errorf("%w: operation %s exists", ErrConflict, id)
		}
		return Operation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(operation_id, revision, incarnation, from_state, to_state, actor, fencings, effects, at) VALUES (?, 1, ?, '', ?, ?, '[]', 'null', ?)`,
		id, l.incarnation, initial, actor, now.Format(time.RFC3339Nano)); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, err
	}
	return Operation{ID: id, Kind: kind, State: initial, Revision: 1, Incarnation: l.incarnation, Data: raw, CreatedAt: now, UpdatedAt: now}, nil
}

// Tx is what a transition's mutation sees: the same SQLite transaction the
// event is appended in, with the ledger's guarded writes exposed.
type Tx struct {
	l   *Ledger
	ctx context.Context
	tx  *sql.Tx
}

// Transition moves an operation from one state to another, atomically with
// the caller's mutations. It refuses if the operation is not in `from`, if
// any fencing lease does not match exactly, or if the mutation fails.
func (l *Ledger) Transition(ctx context.Context, id, from, to, actor string, fencings []Lease, effects any, mutate func(tx *Tx, op *Operation) error) (Event, error) {
	effectsRaw := json.RawMessage("null")
	if effects != nil {
		raw, err := json.Marshal(effects)
		if err != nil {
			return Event{}, err
		}
		effectsRaw = raw
	}
	fencingsRaw, err := json.Marshal(nonNil(fencings))
	if err != nil {
		return Event{}, err
	}
	if err := l.checkForeign(ctx, fencings); err != nil {
		return Event{}, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback()

	var op Operation
	var data, created, updated string
	err = tx.QueryRowContext(ctx, `SELECT kind, state, revision, incarnation, data, created_at, updated_at FROM operations WHERE id = ?`, id).
		Scan(&op.Kind, &op.State, &op.Revision, &op.Incarnation, &data, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, fmt.Errorf("%w: operation %s not found", ErrConflict, id)
	}
	if err != nil {
		return Event{}, err
	}
	op.ID = id
	op.Data = json.RawMessage(data)
	if op.State != from {
		return Event{}, fmt.Errorf("%w: operation %s is %s, not %s", ErrConflict, id, op.State, from)
	}
	for _, lease := range fencings {
		if l.foreign(lease) {
			continue
		}
		if err := l.checkLease(ctx, tx, lease); err != nil {
			return Event{}, err
		}
	}
	if mutate != nil {
		if err := mutate(&Tx{l: l, ctx: ctx, tx: tx}, &op); err != nil {
			return Event{}, err
		}
	}
	now := l.now().UTC()
	revision := op.Revision + 1
	res, err := tx.ExecContext(ctx, `UPDATE operations SET state = ?, revision = ?, incarnation = ?, data = ?, updated_at = ? WHERE id = ? AND revision = ?`,
		to, revision, l.incarnation, string(op.Data), now.Format(time.RFC3339Nano), id, op.Revision)
	if err != nil {
		return Event{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Event{}, fmt.Errorf("%w: operation %s changed underneath", ErrConflict, id)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO events(operation_id, revision, incarnation, from_state, to_state, actor, fencings, effects, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, revision, l.incarnation, from, to, actor, string(fencingsRaw), string(effectsRaw), now.Format(time.RFC3339Nano))
	if err != nil {
		return Event{}, err
	}
	seq, _ := result.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	return Event{Seq: seq, OperationID: id, Revision: revision, Incarnation: l.incarnation, From: from, To: to,
		Actor: actor, Fencings: nonNil(fencings), Effects: effectsRaw, At: now}, nil
}

// Update runs mutations in one transaction outside any operation: for
// bindings that are names rather than state machines, such as which
// project a conversation points at.
func (l *Ledger) Update(ctx context.Context, mutate func(tx *Tx) error) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := mutate(&Tx{l: l, ctx: ctx, tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetData replaces the operation's payload inside the transition.
func (t *Tx) SetData(op *Operation, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	op.Data = raw
	return nil
}

// CompareAndSetName binds a name to an artifact if the name is at exactly
// expectedVersion (0 for "does not exist yet"). Returns the new version.
func (t *Tx) CompareAndSetName(name string, expectedVersion int64, artifact string) (int64, error) {
	var version int64
	err := t.tx.QueryRowContext(t.ctx, `SELECT version FROM names WHERE name = ?`, name).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		version = 0
	} else if err != nil {
		return 0, err
	}
	if version != expectedVersion {
		return 0, fmt.Errorf("%w: name %s is at version %d, expected %d", ErrConflict, name, version, expectedVersion)
	}
	next := version + 1
	now := t.l.now().UTC().Format(time.RFC3339Nano)
	if version == 0 {
		_, err = t.tx.ExecContext(t.ctx, `INSERT INTO names(name, version, artifact, updated_at) VALUES (?, ?, ?, ?)`, name, next, artifact, now)
	} else {
		_, err = t.tx.ExecContext(t.ctx, `UPDATE names SET version = ?, artifact = ?, updated_at = ? WHERE name = ? AND version = ?`, next, artifact, now, name, version)
	}
	if err != nil {
		return 0, err
	}
	return next, nil
}

// PutBinding writes a binding record inside the transition.
func (t *Tx) PutBinding(kind, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO bindings(kind, id, data, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		kind, id, string(raw), t.l.now().UTC().Format(time.RFC3339Nano))
	return err
}

// Exec runs a statement in the transition, for stores that keep their own
// tables in the ledger's database and want their writes to land with the
// event or not at all.
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(t.ctx, query, args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(t.ctx, query, args...)
}

// ---------------------------------------------------------------- reads

// Operation reads the current state.
func (l *Ledger) Operation(ctx context.Context, id string) (Operation, bool, error) {
	var op Operation
	var data, created, updated string
	err := l.db.QueryRowContext(ctx, `SELECT kind, state, revision, incarnation, data, created_at, updated_at FROM operations WHERE id = ?`, id).
		Scan(&op.Kind, &op.State, &op.Revision, &op.Incarnation, &data, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	op.ID = id
	op.Data = json.RawMessage(data)
	op.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	op.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return op, true, nil
}

// Operations lists operations of a kind, optionally in a state, newest first.
func (l *Ledger) Operations(ctx context.Context, kind, state string) ([]Operation, error) {
	return readOperations(ctx, l.db, kind, state)
}

// Operations observes operation facts within the caller's mutation transaction.
func (t *Tx) Operations(kind, state string) ([]Operation, error) {
	return readOperations(t.ctx, t.tx, kind, state)
}

func readOperations(ctx context.Context, source interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, kind, state string) ([]Operation, error) {
	query := `SELECT id, kind, state, revision, incarnation, data, created_at, updated_at FROM operations WHERE kind = ?`
	args := []any{kind}
	if state != "" {
		query += ` AND state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := source.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		var op Operation
		var data, created, updated string
		if err := rows.Scan(&op.ID, &op.Kind, &op.State, &op.Revision, &op.Incarnation, &data, &created, &updated); err != nil {
			return nil, err
		}
		op.Data = json.RawMessage(data)
		op.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		op.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, op)
	}
	return out, rows.Err()
}

// Events returns an operation's history, oldest first.
// RecentEvents pages the whole journal newest first: every transition of
// every operation, before the given sequence (0 = from the end). This is
// what a history view reads; SSE only says "look again".
func (l *Ledger) RecentEvents(ctx context.Context, before int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := l.db.QueryContext(ctx, `SELECT seq, operation_id, revision, incarnation, from_state, to_state, actor, fencings, effects, at FROM events WHERE seq < ? ORDER BY seq DESC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var fencings, at string
		var effects []byte
		if err := rows.Scan(&ev.Seq, &ev.OperationID, &ev.Revision, &ev.Incarnation, &ev.From, &ev.To, &ev.Actor, &fencings, &effects, &at); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(fencings), &ev.Fencings)
		ev.Effects = effects
		ev.At, _ = time.Parse(rfc3339nano, at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (l *Ledger) Events(ctx context.Context, operationID string) ([]Event, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT seq, revision, incarnation, from_state, to_state, actor, fencings, effects, at FROM events WHERE operation_id = ? ORDER BY seq`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var fencings, effects, at string
		if err := rows.Scan(&ev.Seq, &ev.Revision, &ev.Incarnation, &ev.From, &ev.To, &ev.Actor, &fencings, &effects, &at); err != nil {
			return nil, err
		}
		ev.OperationID = operationID
		_ = json.Unmarshal([]byte(fencings), &ev.Fencings)
		ev.Effects = json.RawMessage(effects)
		ev.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// NamedRef is a name's current binding.
type NamedRef struct {
	Name      string    `json:"name"`
	Version   int64     `json:"version"`
	Artifact  string    `json:"artifact"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Name reads a named ref.
func (l *Ledger) Name(ctx context.Context, name string) (NamedRef, bool, error) {
	var ref NamedRef
	var at string
	err := l.db.QueryRowContext(ctx, `SELECT version, artifact, updated_at FROM names WHERE name = ?`, name).Scan(&ref.Version, &ref.Artifact, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return NamedRef{}, false, nil
	}
	if err != nil {
		return NamedRef{}, false, err
	}
	ref.Name = name
	ref.UpdatedAt, _ = time.Parse(time.RFC3339Nano, at)
	return ref, true, nil
}

// Names lists refs under a prefix.
func (l *Ledger) Names(ctx context.Context, prefix string) ([]NamedRef, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT name, version, artifact, updated_at FROM names WHERE name LIKE ? ORDER BY name`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NamedRef
	for rows.Next() {
		var ref NamedRef
		var at string
		if err := rows.Scan(&ref.Name, &ref.Version, &ref.Artifact, &at); err != nil {
			return nil, err
		}
		ref.UpdatedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, ref)
	}
	return out, rows.Err()
}

// GetBinding reads a binding record into out.
func (l *Ledger) GetBinding(ctx context.Context, kind, id string, out any) (bool, error) {
	var data string
	err := l.db.QueryRowContext(ctx, `SELECT data FROM bindings WHERE kind = ? AND id = ?`, kind, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(data), out)
}

// PutBinding writes a binding record outside any transition.
func (l *Ledger) PutBinding(ctx context.Context, kind, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = l.db.ExecContext(ctx, `INSERT INTO bindings(kind, id, data, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		kind, id, string(raw), l.now().UTC().Format(time.RFC3339Nano))
	return err
}

// DeleteBinding removes a binding record.
func (l *Ledger) DeleteBinding(ctx context.Context, kind, id string) error {
	_, err := l.db.ExecContext(ctx, `DELETE FROM bindings WHERE kind = ? AND id = ?`, kind, id)
	return err
}

// Bindings lists every record of a kind as raw JSON, keyed by id.
func (l *Ledger) Bindings(ctx context.Context, kind string) (map[string]json.RawMessage, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id, data FROM bindings WHERE kind = ?`, kind)
	return scanBindings(rows, err)
}

// Bindings reads the binding set in the same transaction as a mutation.
// Domains can validate cross-record invariants before publishing changes.
func (t *Tx) Bindings(kind string) (map[string]json.RawMessage, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT id, data FROM bindings WHERE kind = ?`, kind)
	return scanBindings(rows, err)
}

func scanBindings(rows *sql.Rows, err error) (map[string]json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		out[id] = json.RawMessage(data)
	}
	return out, rows.Err()
}

// LeaseOf reads a resource's current lease, held or not.
func (l *Ledger) LeaseOf(ctx context.Context, key string) (Lease, bool, error) {
	var lease Lease
	var expires string
	err := l.db.QueryRowContext(ctx, `SELECT incarnation, epoch, holder, expires_at FROM leases WHERE resource_key = ?`, key).
		Scan(&lease.Incarnation, &lease.Epoch, &lease.Holder, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	lease.Key = key
	lease.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	return lease, true, nil
}

func errNoRows() error { return sql.ErrNoRows }

func nonNil(leases []Lease) []Lease {
	if leases == nil {
		return []Lease{}
	}
	return leases
}
