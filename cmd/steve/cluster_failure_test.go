package main

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/coordination"
)

func TestApplicationStoreUncertaintyRebuildsOnlyBusinessGeneration(t *testing.T) {
	options, _ := testPeerOptions(t, clusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	peer, err := openClusterPeer(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	first := waitPeerReady(t, peer)
	worker := peer.Worker()
	cause := fmt.Errorf("MCP durable state unavailable: %w", coordination.ErrUnavailable)
	peer.applicationStoreFailure(first, cause)
	if first.Context.Err() == nil {
		t.Fatal("uncertain cached authorization remained active")
	}
	if peer.runtime.Load().Status().Closed {
		t.Fatal("transient application store failure permanently stopped consensus peer")
	}
	next := waitPeerReady(t, peer)
	if next.Generation <= first.Generation || next.WriterGeneration <= first.WriterGeneration || next.Ledger == first.Ledger {
		t.Fatal("application rebuilt without new stores and a durable writer fence")
	}
	if peer.Worker() != worker {
		t.Fatal("application store recovery replaced the physical execution service")
	}
	peer.applicationStoreFailure(first, errors.New("late failure from old MCP instance"))
	if next.Context.Err() != nil {
		t.Fatal("late old-generation callback interrupted the replacement")
	}
}
