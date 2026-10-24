package acphost

import (
	"context"
	"testing"
)

// A workspace callback the kill cannot end keeps the host undrained: the
// agent being gone says nothing of a write still in flight.
func TestHostCallbacksDrainedWaitsForABlockedFileCallback(t *testing.T) {
	h := New(Config{NoRestart: true})
	defer h.Close()
	if !h.CallbacksDrained() {
		t.Fatal("idle host reports a running callback")
	}
	blocked := &workspaceFileCall{class: "file", cancel: func() {}}
	h.mu.Lock()
	h.fileCalls = map[*workspaceFileCall]struct{}{blocked: {}}
	h.mu.Unlock()
	if err := h.Kill(context.Background()); err == nil {
		t.Fatal("kill confirmed a stop with a callback still running")
	}
	if h.CallbacksDrained() || h.AllProcessesStopped() {
		t.Fatal("blocked callback was taken for drained")
	}
	h.mu.Lock()
	delete(h.fileCalls, blocked)
	h.mu.Unlock()
	if !h.CallbacksDrained() {
		t.Fatal("finished callback still reported running")
	}
}
