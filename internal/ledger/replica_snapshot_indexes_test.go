package ledger

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"modernc.org/sqlite"
)

func snapshotIndexSchema(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name, COALESCE(sql, '') FROM sqlite_schema WHERE type='index'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatal(err)
		}
		out[name] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func snapshotForIndexTest(t *testing.T, book *Ledger, checkpoint bool) []byte {
	t.Helper()
	if checkpoint {
		raw, _ := checkpointReplica(t, book, 1)
		return raw
	}
	raw, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReplicaSnapshotExportsNoRegisteredReadIndexes(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		name := "snapshot"
		if checkpoint {
			name = "checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			book, _ := replicaBook(t)
			for i := range 3 {
				if err := book.PutBinding(t.Context(), "task-attempt", "task:0", i); err != nil {
					t.Fatal(err)
				}
			}
			// Unregistered indexes, including uniqueness constraints, are not
			// ours to remove. The exported primary keys must remain as well.
			if _, err := book.db.Exec(`CREATE UNIQUE INDEX bindings_export_unique ON bindings(kind,id)`); err != nil {
				t.Fatal(err)
			}
			before := snapshotIndexSchema(t, book.db)
			raw := snapshotForIndexTest(t, book, checkpoint)
			if after := snapshotIndexSchema(t, book.db); !reflect.DeepEqual(before, after) {
				t.Fatalf("snapshot changed live indexes: before=%v after=%v", before, after)
			}
			if got := receiptCount(t, book); got != 3 {
				t.Fatalf("snapshot changed live receipts: %d", got)
			}
			_, database, err := decodeReplicaSnapshot(raw)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "export.db")
			if err := os.WriteFile(path, database, 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			exported := snapshotIndexSchema(t, db)
			readIndexesMu.Lock()
			registered := make(map[string]bool, len(readIndexes))
			for name := range readIndexes {
				registered[name] = true
			}
			readIndexesMu.Unlock()
			for name, definition := range before {
				got, exists := exported[name]
				if registered[name] {
					if exists {
						t.Errorf("export contains registered local index %s", name)
					}
				} else if !exists || got != definition {
					t.Errorf("export changed non-derived index %s", name)
				}
			}
			if _, err := db.Exec(`INSERT INTO bindings(kind,id,data,updated_at) SELECT kind,id,data,updated_at FROM bindings`); err == nil {
				t.Fatal("export lost binding uniqueness")
			}
			target, err := Open(t.TempDir(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			if err := target.RestoreReplica(raw); err != nil {
				t.Fatal(err)
			}
			restored := snapshotIndexSchema(t, target.db)
			for name := range registered {
				if restored[name] != before[name] {
					t.Errorf("receiver did not rebuild local index %s", name)
				}
			}
			wantReceipts := 3
			if checkpoint {
				wantReceipts = 2
			}
			if got := receiptCount(t, target); got != wantReceipts {
				t.Fatalf("restored receipts=%d want=%d", got, wantReceipts)
			}
			var value int
			if ok, err := target.GetBinding(t.Context(), "task-attempt", "task:0", &value); err != nil || !ok || value != 2 {
				t.Fatalf("restored task accounting=%d found=%v err=%v", value, ok, err)
			}
		})
	}
}

func TestReplicaSnapshotRefusesConstraintAtReadIndexName(t *testing.T) {
	book, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if _, err := book.db.Exec(`DROP INDEX events_history_time_seq`); err != nil {
		t.Fatal(err)
	}
	if _, err := book.db.Exec(`CREATE UNIQUE INDEX events_history_time_seq ON events(seq)`); err != nil {
		t.Fatal(err)
	}
	before := snapshotIndexSchema(t, book.db)
	if _, err := book.SnapshotReplica(); err == nil {
		t.Fatal("snapshot silently removed a uniqueness constraint at a registered name")
	}
	if after := snapshotIndexSchema(t, book.db); !reflect.DeepEqual(before, after) {
		t.Fatal("failed snapshot changed live indexes")
	}
}

// The helper processes deliberately have different function/index registries.
// A same-process restore would conceal a function missing from an older binary.
func TestReplicaSnapshotWithoutSenderFunction(t *testing.T) {
	if role := os.Getenv("STEVE_SNAPSHOT_INDEX_HELPER"); role != "" {
		runSnapshotIndexHelper(t, role, os.Getenv("STEVE_SNAPSHOT_INDEX_DIR"), os.Getenv("STEVE_SNAPSHOT_INDEX_CHECKPOINT") == "1")
		return
	}
	for _, checkpoint := range []string{"0", "1"} {
		t.Run("checkpoint="+checkpoint, func(t *testing.T) {
			dir := t.TempDir()
			for _, role := range []string{"producer", "consumer"} {
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReplicaSnapshotWithoutSenderFunction$", "-test.count=1")
				cmd.Env = append(os.Environ(), "STEVE_SNAPSHOT_INDEX_HELPER="+role, "STEVE_SNAPSHOT_INDEX_DIR="+dir, "STEVE_SNAPSHOT_INDEX_CHECKPOINT="+checkpoint)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s: %v\n%s", role, err, output)
				}
			}
		})
	}
}

func runSnapshotIndexHelper(t *testing.T, role, dir string, checkpoint bool) {
	t.Helper()
	const function = "snapshot_sender_task_turn"
	const index = "bindings_snapshot_sender"
	if role == "producer" {
		sqlite.MustRegisterDeterministicScalarFunction(function, 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			return args[0], nil
		})
		MustRegisterReadIndex(index, `CREATE INDEX IF NOT EXISTS `+index+` ON bindings(`+function+`(id,data)) WHERE kind='task-attempt'`)
		book, replica := replicaBook(t)
		for i := range 3 {
			if err := book.PutBinding(t.Context(), "task-attempt", "task:0", map[string]any{"turn_id": "input", "index": i}); err != nil {
				t.Fatal(err)
			}
		}
		before := snapshotIndexSchema(t, book.db)
		raw := snapshotForIndexTest(t, book, checkpoint)
		if after := snapshotIndexSchema(t, book.db); !reflect.DeepEqual(before, after) {
			t.Fatal("export changed sender live indexes")
		}
		if err := os.WriteFile(filepath.Join(dir, "snapshot"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := book.PutBinding(t.Context(), "task-attempt", "task:0", map[string]any{"turn_id": "next-input", "index": 3}); err != nil {
			t.Fatal(err)
		}
		writes, err := json.Marshal(replica.writes)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "writes"), writes, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if role != "consumer" {
		t.Fatalf("unknown helper role %q", role)
	}
	target, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	var probe string
	if err := target.db.QueryRow(`SELECT ` + function + `('id','data')`).Scan(&probe); err == nil {
		t.Fatal("consumer unexpectedly supports sender function")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if err := target.RestoreReplica(raw); err != nil {
		t.Fatalf("consumer restore: %v", err)
	}
	writesRaw, err := os.ReadFile(filepath.Join(dir, "writes"))
	if err != nil {
		t.Fatal(err)
	}
	var writes []ReplicatedWrite
	if err := json.Unmarshal(writesRaw, &writes); err != nil {
		t.Fatal(err)
	}
	wantReceipts := 3
	if checkpoint {
		wantReceipts = 2
	}
	if got := receiptCount(t, target); got != wantReceipts {
		t.Fatalf("consumer receipts=%d want=%d", got, wantReceipts)
	}
	// A retained receipt must still replay, and the next task mutation must
	// apply without needing the sender's expression-index function.
	for _, write := range writes[2:] {
		if _, err := target.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload); err != nil {
			t.Fatalf("consumer apply task mutation: %v", err)
		}
	}
	var value struct {
		TurnID string `json:"turn_id"`
		Index  int    `json:"index"`
	}
	if ok, err := target.GetBinding(t.Context(), "task-attempt", "task:0", &value); err != nil || !ok || value.TurnID != "next-input" || value.Index != 3 {
		t.Fatalf("consumer task accounting=%+v found=%v err=%v", value, ok, err)
	}
}

func TestRestoreGenerationMovesAcrossEveryRestore(t *testing.T) {
	source, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	before := target.RestoreGeneration()
	if before%2 != 0 {
		t.Fatalf("idle generation %d reads as a restore in progress", before)
	}
	snapshot, err := source.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	if err := target.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	if after := target.RestoreGeneration(); after == before || after%2 != 0 {
		t.Fatalf("generation %d -> %d across a restore", before, after)
	}
}
