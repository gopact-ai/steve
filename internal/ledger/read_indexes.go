package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Read indexes are derived local schema, not replicated business mutations.
// Domain owners register their exact predicates during package initialization;
// every database entry point installs the same definitions, including staging
// databases restored from snapshots. No read query silently falls back to scans.
var readIndexes = map[string]string{}
var readIndexName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var readIndexesMu sync.Mutex
var readIndexesOpen bool

// stripSnapshotReadIndexes removes only registered, non-unique read indexes
// from a private snapshot copy. Receivers install their own supported versions;
// shipping expression indexes would require the sender's SQL functions there.
// Never call this on a live ledger.
func stripSnapshotReadIndexes(db *sql.DB) error {
	readIndexesMu.Lock()
	names := slices.Sorted(maps.Keys(readIndexes))
	readIndexesMu.Unlock()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, name := range names {
		var unique int
		var origin string
		err := tx.QueryRow(`SELECT i."unique", i.origin
			FROM sqlite_schema AS s JOIN pragma_index_list(s.tbl_name) AS i ON i.name=s.name
			WHERE s.type='index' AND s.name=?`, name).Scan(&unique, &origin)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect snapshot read index %s: %w", name, err)
		}
		if unique != 0 || origin != "c" {
			return fmt.Errorf("ledger: snapshot read index %s is a constraint", name)
		}
		if _, err := tx.Exec(`DROP INDEX "` + name + `"`); err != nil {
			return fmt.Errorf("remove snapshot read index %s: %w", name, err)
		}
	}
	return tx.Commit()
}

// MustRegisterReadIndex is startup-only, before any ledger connections open.
// The definition must be one CREATE INDEX statement and must be deterministic.
// An owner that changes a custom SQL function's semantics must version its name.
func MustRegisterReadIndex(name, definition string) {
	readIndexesMu.Lock()
	defer readIndexesMu.Unlock()
	if readIndexesOpen {
		panic("ledger: read indexes must be registered before opening a database")
	}
	if !readIndexName.MatchString(name) || !strings.HasPrefix(definition, "CREATE INDEX IF NOT EXISTS "+name+" ON ") || strings.Contains(definition, ";") {
		panic("ledger: invalid read index definition")
	}
	if _, exists := readIndexes[name]; exists {
		panic("ledger: duplicate read index " + name)
	}
	readIndexes[name] = definition
}

func ensureReadIndexes(db *sql.DB) error {
	readIndexesMu.Lock()
	readIndexesOpen = true
	definitions := maps.Clone(readIndexes)
	readIndexesMu.Unlock()
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	slices.Sort(names)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, name := range names {
		definition := definitions[name]
		var existing string
		err := tx.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='index' AND name=?`, name).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// SQLite strips IF NOT EXISTS from its stored CREATE statement.
		expected := strings.Replace(definition, "IF NOT EXISTS ", "", 1)
		if err == nil && strings.Join(strings.Fields(existing), " ") != strings.Join(strings.Fields(expected), " ") {
			if _, err := tx.Exec(`DROP INDEX "` + name + `"`); err != nil {
				return fmt.Errorf("replace read index %s: %w", name, err)
			}
		}
		if _, err := tx.Exec(definition); err != nil {
			return fmt.Errorf("install read index %s: %w", name, err)
		}
	}
	return tx.Commit()
}
