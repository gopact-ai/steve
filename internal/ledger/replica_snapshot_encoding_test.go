package ledger

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestReplicaSnapshotStoresDatabaseWithoutBase64Expansion(t *testing.T) {
	book, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if err := book.Document("large").Save(bytes.Repeat([]byte("x"), 1<<20)); err != nil {
		t.Fatal(err)
	}
	raw, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	var pages, pageSize int
	if err := book.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := book.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if len(raw) > pages*pageSize+128 {
		t.Fatalf("snapshot expanded SQLite payload: database=%d snapshot=%d", pages*pageSize, len(raw))
	}
	if json.Valid(raw) {
		t.Fatal("database still encoded through JSON")
	}
	restored, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.RestoreReplica(raw); err != nil {
		t.Fatal(err)
	}
	got, ok, err := restored.Document("large").Load()
	if err != nil || !ok || !bytes.Equal(got, bytes.Repeat([]byte("x"), 1<<20)) {
		t.Fatalf("restored payload differs: found=%v size=%d err=%v", ok, len(got), err)
	}
}

func TestReplicaSnapshotRejectsDamagedEnvelopeBeforeChangingState(t *testing.T) {
	source, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	raw, err := source.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	target, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.Document("sentinel").Save([]byte("unchanged")); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Clone(raw)
	changed[len(changed)/2] ^= 1
	for name, input := range map[string][]byte{
		"empty": nil, "short": raw[:8], "truncated": raw[:len(raw)-1],
		"trailing": append(bytes.Clone(raw), 1), "corrupt": changed,
		"old-format": []byte(`{"format":1,"incarnation":1,"database":"eA=="}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := target.RestoreReplica(input); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
			got, ok, err := target.Document("sentinel").Load()
			if err != nil || !ok || string(got) != "unchanged" {
				t.Fatalf("invalid snapshot changed state: %q %v %v", got, ok, err)
			}
		})
	}
}

func BenchmarkReplicaSnapshotImage(b *testing.B) {
	book, err := Open(b.TempDir(), Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer book.Close()
	if err := book.Document("large").Save(bytes.Repeat([]byte("x"), 8<<20)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(8 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := book.SnapshotReplica(); err != nil {
			b.Fatal(err)
		}
	}
}
