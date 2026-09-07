package acphost

import (
	"context"
	"testing"
)

type countedProcessStarts struct {
	inner  Transport
	starts int
}

func (tr *countedProcessStarts) Name() string { return "counted-test-agent" }
func (tr *countedProcessStarts) Start(ctx context.Context) (Process, error) {
	tr.starts++
	return tr.inner.Start(ctx)
}

func TestNoRestartHostCannotStartReplacementForAnOldSession(t *testing.T) {
	transport := &countedProcessStarts{inner: LocalTransport{Command: buildMockAgent(t), ProcessDir: t.TempDir()}}
	h := New(Config{Transport: transport, NoRestart: true})
	defer h.Close()
	id, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h.Stop()
	if _, _, err := h.Prompt(t.Context(), id, generation, "must not restart", nil); err == nil {
		t.Fatal("stopped native session accepted prompt")
	}
	if transport.starts != 1 {
		t.Fatalf("old session started %d native processes", transport.starts)
	}
	if _, _, err := h.OpenSession(t.Context(), id, SessionConfig{Workdir: t.TempDir()}); err == nil {
		t.Fatal("old session reopened a native process")
	}
	if transport.starts != 1 {
		t.Fatalf("open silently replaced native process: %d", transport.starts)
	}
}
