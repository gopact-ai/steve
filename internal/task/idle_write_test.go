package task

import "testing"

// A write that finds nothing to change leaves the ledger alone: it does
// not open a write transaction, which in a cluster costs a round of
// consensus even when nothing is committed.
func TestAWriteThatChangesNothingOpensNoLedgerWrite(t *testing.T) {
	s, book := taskRecordBook(t)
	child, err := s.Create(Task{Goal: "child", Member: "builder", Channel: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDelivery(child.ID, DeliveryDelivered); err != nil {
		t.Fatal(err)
	}
	replicated := &taskReplicator{book: book}
	if err := book.AttachReplication(replicated); err != nil {
		t.Fatal(err)
	}
	revision := s.revision
	// A delivered result is never made queued again, so this changes nothing.
	if err := s.RecordDelivery([]string{child.ID}, DeliveryQueued, ""); err != nil {
		t.Fatal(err)
	}
	if replicated.prepared != 0 || len(replicated.payloads) != 0 || s.revision != revision {
		t.Fatalf("a write that changed nothing opened %d write transactions and committed %d; revision %d -> %d", replicated.prepared, len(replicated.payloads), revision, s.revision)
	}
}
