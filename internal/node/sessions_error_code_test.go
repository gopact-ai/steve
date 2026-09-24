package node

import (
	"context"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// Every command error a node records carries its code, including a
// command the node stopped before dispatching it.
func TestCommandStoppedBeforeDispatchCarriesAnErrorCode(t *testing.T) {
	server := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir()})
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	one := &ownedSession{service: &SessionService{server: server, ctx: stopped}, changed: make(chan struct{}), record: sessionRecord{Format: 1, State: nodewire.SessionState{ID: "ns_" + strings.Repeat("b", 64), State: nodewire.SessionRunning, InputAccepted: 1, Binding: nodeSessionRequest("prompt").Binding}, CurrentCommand: "input", Commands: map[string]nodewire.SessionCommand{"input": {ID: "input", InputSequence: 1, State: nodewire.SessionCommandAccepted, DispatchState: "not-dispatched"}}, CommandHashes: map[string]string{"input": "hash"}}}
	if err := one.commitLocked(one.record); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(one.service.closeRecords)
	one.run(nodewire.SessionRequest{CommandID: "input"})
	one.mu.Lock()
	command := one.record.Commands["input"]
	one.mu.Unlock()
	if command.State != nodewire.SessionCommandUncertain || command.Error == "" || command.ErrorCode != nodewire.SessionErrorFailed {
		t.Fatalf("command = %+v, want an uncertain command whose error is coded %q", command, nodewire.SessionErrorFailed)
	}
}
