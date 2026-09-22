package ledger

import (
	"strings"
	"testing"
)

func TestReplicaRestoreInstallsRequiredReadIndexes(t *testing.T) {
	source, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.db.Exec("DROP INDEX events_history_time_seq"); err != nil {
		t.Fatal(err)
	}
	raw, err := source.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.RestoreReplica(raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := target.HistoryEvents(t.Context(), nil, nil, 1); err != nil {
		t.Fatalf("restored replica accepted an unusable read schema: %v", err)
	}
	writer, err := Open(dir, Options{ReplicaWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, _, err := writer.HistoryEvents(t.Context(), nil, nil, 1); err != nil {
		t.Fatalf("replica writer cannot read restored index: %v", err)
	}
}

func TestReplicaWriterRepairsIncorrectReadIndexDefinition(t *testing.T) {
	dir := t.TempDir()
	book, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if err := book.requireReplication(); err != nil {
		t.Fatal(err)
	}
	if _, err := book.db.Exec("DROP INDEX events_history_time_seq"); err != nil {
		t.Fatal(err)
	}
	if _, err := book.db.Exec("CREATE INDEX events_history_time_seq ON events(seq)"); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(dir, Options{ReplicaWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var definition string
	if err := writer.db.QueryRow("SELECT sql FROM sqlite_schema WHERE name='events_history_time_seq'").Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(definition, "rtrim(at, 'Z')") {
		t.Fatalf("wrong read index was trusted: %s", definition)
	}
}
