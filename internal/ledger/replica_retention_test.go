package ledger

import (
	"errors"
	"fmt"
	"testing"
)

type snapshotWithFloor interface {
	SnapshotReplicaCheckpoint(uint64) ([]byte, func() error, error)
}

func checkpointReplica(t *testing.T, book *Ledger, floor uint64) ([]byte, func() error) {
	t.Helper()
	owner, ok := any(book).(snapshotWithFloor)
	if !ok {
		t.Fatal("replica cannot checkpoint an explicitly retained replay suffix")
	}
	data, persisted, err := owner.SnapshotReplicaCheckpoint(floor)
	if err != nil {
		t.Fatal(err)
	}
	return data, persisted
}

func receiptCount(t *testing.T, book *Ledger) int {
	t.Helper()
	var count int
	if err := book.DB().QueryRow(`SELECT COUNT(*) FROM replica_commands`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestReplicaCheckpointRetainsReceiptsUntilPersistenceAndRestoresSuffix(t *testing.T) {
	book, replica := replicaBook(t)
	for i := range 12 {
		if err := book.PutBinding(t.Context(), "fact", "current", i); err != nil {
			t.Fatal(err)
		}
	}
	// Neither capture nor an abandoned snapshot is permission to delete.
	data, persisted := checkpointReplica(t, book, 8)
	if got := receiptCount(t, book); got != 12 {
		t.Fatalf("snapshot capture deleted live replay evidence: %d", got)
	}
	if err := book.PutBinding(t.Context(), "fact", "current", 12); err != nil {
		t.Fatal(err)
	}
	if err := persisted(); err != nil {
		t.Fatal(err)
	}
	if err := persisted(); err != nil {
		t.Fatal(err)
	}
	if got := receiptCount(t, book); got != 5 {
		t.Fatalf("checkpoint removed new writes or retained old receipts: %d", got)
	}
	target, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.RestoreReplica(data); err != nil {
		t.Fatal(err)
	}
	if got := receiptCount(t, target); got != 4 {
		t.Fatalf("snapshot does not contain the exact retained suffix: %d", got)
	}
	for _, write := range replica.writes[8:] {
		if _, err := target.ApplyReplicated(write.ID, write.ExpectedVersion+1, write.Payload); err != nil {
			t.Fatalf("retained replay failed: %v", err)
		}
	}
	var value int
	ok, err := target.GetBinding(t.Context(), "fact", "current", &value)
	if err != nil || !ok || value != 12 {
		t.Fatalf("replay duplicated or lost business state: %d %v %v", value, ok, err)
	}
	old := replica.writes[0]
	if _, err := target.ApplyReplicated(old.ID, 1, old.Payload); err == nil {
		t.Fatal("expired committed replay was silently accepted")
	}
}

func TestOldCheckpointCallbackCannotPruneRestoredGeneration(t *testing.T) {
	book, _ := replicaBook(t)
	for i := range 10 {
		if err := book.PutBinding(t.Context(), "fact", "current", i); err != nil {
			t.Fatal(err)
		}
	}
	before, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	_, persisted := checkpointReplica(t, book, 8)
	if err := book.RestoreReplica(before); err != nil {
		t.Fatal(err)
	}
	if err := persisted(); err != nil {
		t.Fatal(err)
	}
	if got := receiptCount(t, book); got != 10 {
		t.Fatalf("old snapshot callback pruned restored replay evidence: %d", got)
	}
}

func TestReplicaCheckpointRejectsInvalidFloorAndMissingSuffix(t *testing.T) {
	book, _ := replicaBook(t)
	for i := range 6 {
		if err := book.PutBinding(t.Context(), "fact", "current", i); err != nil {
			t.Fatal(err)
		}
	}
	owner, ok := any(book).(snapshotWithFloor)
	if !ok {
		t.Fatal("replica checkpoint owner is missing")
	}
	if _, _, err := owner.SnapshotReplicaCheckpoint(7); err == nil {
		t.Fatal("future replay floor accepted")
	}
	// Corrupt only a disposable database, not the mutation API.
	if _, err := book.db.Exec(`DELETE FROM replica_commands WHERE version = 5`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := owner.SnapshotReplicaCheckpoint(3); err == nil {
		t.Fatal("checkpoint hid a gap in the retained suffix")
	}
}

func TestReplicaCheckpointFailureDoesNotAdvanceLiveFloor(t *testing.T) {
	book, _ := replicaBook(t)
	for i := range 6 {
		if err := book.PutBinding(t.Context(), "fact", "current", i); err != nil {
			t.Fatal(err)
		}
	}
	_, persisted := checkpointReplica(t, book, 3)
	if _, err := book.db.Exec(`CREATE TRIGGER reject_prune BEFORE DELETE ON replica_commands BEGIN SELECT RAISE(ABORT,'prune unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := persisted(); err == nil {
		t.Fatal("prune failure was hidden")
	}
	if got := receiptCount(t, book); got != 6 {
		t.Fatalf("failed prune removed evidence: %d", got)
	}
	var floor uint64
	if err := book.db.QueryRow(`SELECT replay_floor FROM replica_state WHERE singleton=1`).Scan(&floor); err != nil || floor != 0 {
		t.Fatalf("failed prune advanced floor: %d %v", floor, err)
	}
	if _, err := book.db.Exec(`DROP TRIGGER reject_prune`); err != nil {
		t.Fatal(err)
	}
	if err := persisted(); err != nil {
		t.Fatal(err)
	}
	if got := receiptCount(t, book); got != 3 {
		t.Fatal(fmt.Errorf("retry did not prune exact suffix: %d", got))
	}
	if _, err := book.replicationState(); errors.Is(err, ErrReplicaFailed) {
		t.Fatal("optional physical compaction poisoned the replica")
	}
}

func TestReplicaReceiptIdentityIsVersionScopedAcrossPhysicalFloors(t *testing.T) {
	source, writes := replicaBook(t)
	for i := range 3 {
		if err := source.PutBinding(t.Context(), "fact", "current", i); err != nil {
			t.Fatal(err)
		}
	}
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			book, err := Open(t.TempDir(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			for i, write := range writes.writes {
				if _, err := book.ApplyReplicated("same-scoped-id", uint64(i+1), write.Payload); err != nil {
					t.Fatal(err)
				}
				if compact && i == 0 {
					_, persisted := checkpointReplica(t, book, 1)
					if err := persisted(); err != nil {
						t.Fatal(err)
					}
				}
			}
			var value int
			if ok, err := book.GetBinding(t.Context(), "fact", "current", &value); err != nil || !ok || value != 2 {
				t.Fatalf("different physical floors changed application: %d %v %v", value, ok, err)
			}
			last := writes.writes[2]
			if err := book.confirmReplicaWrite("same-scoped-id", 3, last.Payload); err != nil {
				t.Fatal(err)
			}
			_, persisted := checkpointReplica(t, book, 2)
			if err := persisted(); err != nil {
				t.Fatal(err)
			}
			if err := book.confirmReplicaWrite("same-scoped-id", 1, writes.writes[0].Payload); !errors.Is(err, ErrReplayExpired) {
				t.Fatalf("late acknowledgement not explicitly expired: %v", err)
			}
			if _, err := book.replicationState(); err != nil {
				t.Fatalf("late acknowledgement poisoned replica: %v", err)
			}
		})
	}
}
