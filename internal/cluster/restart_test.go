package cluster

import (
	"context"
	"testing"
	"time"
)

func TestRequestedRestartRebuildsWriterWithoutChangingCoordinator(t *testing.T) {
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	first := ready(t, r)
	if err := first.Ledger.Document("restart-check").Save([]byte(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := r.RestartGeneration(first.Generation); err != nil {
		t.Fatal(err)
	}
	if first.Context.Err() == nil {
		t.Fatal("restart did not immediately revoke the old application")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	next, err := r.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation <= first.Generation || next.WriterGeneration <= first.WriterGeneration || next.Assignment != first.Assignment {
		t.Fatalf("restart changed wrong authority: first=%+v next=%+v", first.Assignment, next.Assignment)
	}
	if err := first.Ledger.Document("restart-check").Save([]byte(`{"value":2}`)); err == nil {
		t.Fatal("old writer remained writable")
	}
	raw, ok, err := next.Ledger.Document("restart-check").Load()
	if err != nil || !ok || string(raw) != `{"value":1}` {
		t.Fatalf("restart lost durable state: %s %t %v", raw, ok, err)
	}
	if err := r.RestartGeneration(first.Generation); err == nil {
		t.Fatal("stale restart was accepted")
	}
	if next.Context.Err() != nil {
		t.Fatal("stale restart invalidated current application")
	}
}
