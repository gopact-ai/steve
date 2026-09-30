package acphost

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
)

type forceProcess struct {
	killed  atomic.Bool
	settled chan struct{}
	cause   error
}

func (p *forceProcess) Stdout() io.ReadCloser   { return io.NopCloser(strings.NewReader("")) }
func (p *forceProcess) Stdin() io.WriteCloser   { return forceInput{} }
func (p *forceProcess) Wait() error             { return nil }
func (p *forceProcess) Exited() <-chan struct{} { return p.settled }
func (p *forceProcess) Kill()                   {}
func (p *forceProcess) Stopped() bool           { return p.killed.Load() }
func (p *forceProcess) KillNow(context.Context) error {
	if p.cause != nil {
		return p.cause
	}
	p.killed.Store(true)
	close(p.settled)
	return nil
}

type forceInput struct{}

func (forceInput) Write(b []byte) (int, error) { return len(b), nil }
func (forceInput) Close() error                { return nil }

func TestHostKillSkipsGraceAndVisitsEveryGeneration(t *testing.T) {
	h := New(Config{})
	a := &forceProcess{settled: make(chan struct{})}
	b := &forceProcess{settled: make(chan struct{})}
	h.processes = map[uint64]Process{1: a, 2: b}
	h.settling = map[uint64]chan struct{}{1: a.settled, 2: b.settled}
	killer, ok := any(h).(interface{ Kill(context.Context) error })
	if !ok {
		t.Fatal("host has no immediate kill")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := killer.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.killed.Load() || !b.killed.Load() || !h.AllProcessesStopped() {
		t.Fatal("kill did not settle all generations")
	}
	if !h.isClosed {
		t.Fatal("kill left host able to restart")
	}
}

func TestHostKillKeepsUnprovenGroupUnconfirmed(t *testing.T) {
	h := New(Config{})
	p := &forceProcess{settled: make(chan struct{}), cause: procgroup.ErrUnproven}
	h.processes = map[uint64]Process{1: p}
	killer, ok := any(h).(interface{ Kill(context.Context) error })
	if !ok {
		t.Fatal("host has no immediate kill")
	}
	if err := killer.Kill(t.Context()); !errors.Is(err, procgroup.ErrUnproven) {
		t.Fatalf("kill = %v", err)
	}
	if h.AllProcessesStopped() {
		t.Fatal("unproven group was confirmed")
	}
}
