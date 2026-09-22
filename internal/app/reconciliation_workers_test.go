package app

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoveryWorkersCloseAdmissionBeforeJoin(t *testing.T) {
	w := &reconciliationWorkers{}
	release := make(chan struct{})
	if !w.Go(func() { <-release }) {
		t.Fatal("live owner refused work")
	}
	closed := make(chan struct{})
	go func() { w.Close(); close(closed) }()
	deadline := time.After(time.Second)
	for {
		w.mu.Lock()
		closing := w.closing
		w.mu.Unlock()
		if closing {
			break
		}
		select {
		case <-deadline:
			close(release)
			t.Fatal("shutdown never closed admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	var calls atomic.Int32
	var senders sync.WaitGroup
	for range 30 {
		senders.Go(func() {
			if w.Go(func() { calls.Add(1) }) {
				t.Error("worker admitted after shutdown started")
			}
		})
	}
	senders.Wait()
	select {
	case <-closed:
		t.Fatal("shutdown did not join its accepted worker")
	default:
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown never joined")
	}
	if w.Go(func() { calls.Add(1) }) || calls.Load() != 0 {
		t.Fatal("closed empty group accepted a new worker")
	}
}
