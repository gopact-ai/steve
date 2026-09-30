//go:build linux || darwin

package acphost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
	"github.com/gopact-ai/steve/internal/view"
)

// exitingPrompt is a prompt the agent does not end on its own, on a host
// whose check of the agent's process group finds a member running for as
// long as held says so, as one the kill has not ended yet.
type exitingPrompt struct {
	h          *Host
	generation uint64
	leader     int
	done       chan error
}

func startExitingPrompt(t *testing.T, ctx context.Context, held func() bool) *exitingPrompt {
	t.Helper()
	calls := kernelGroup
	calls.inspect = func(group int) (procgroup.Remains, error) {
		if held() {
			return procgroup.Remains{Running: 1}, nil
		}
		return procgroup.Inspect(group)
	}
	var leader atomic.Int64
	h := New(Config{Transport: LocalTransport{
		Command: buildMockAgent(t), ProcessDir: t.TempDir(),
		Started: func(id procgroup.Identity) { leader.Store(int64(id.Leader)) }, group: &calls,
	}, NoRestart: true})
	t.Cleanup(h.Close)
	opening, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	sid, generation, err := h.OpenSession(opening, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := &exitingPrompt{h: h, generation: generation, leader: int(leader.Load()), done: make(chan error, 1)}
	started := make(chan struct{})
	var once sync.Once
	go func() {
		_, _, err := h.Prompt(ctx, sid, generation, "ignore-cancel", func(view.Progress) { once.Do(func() { close(started) }) })
		p.done <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not start the prompt")
	}
	return p
}

// exit kills the agent mid-prompt and returns how long after the kill the
// prompt returned, and what it returned.
func (p *exitingPrompt) exit(t *testing.T) (time.Duration, error) {
	t.Helper()
	began := time.Now()
	if err := syscall.Kill(p.leader, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		return time.Since(began), err
	case <-time.After(exitedGroupWait + 10*time.Second):
		t.Fatal("the prompt of an agent that exited did not return")
		return 0, nil
	}
}

// An agent that exits mid-prompt leaves its process group to the transport,
// which kills what is left in it; the group empties a moment later. That
// ends the prompt as an ordinary failure with the stop confirmed, not as a
// stop nobody saw.
func TestHostConfirmsTheStopOfAnAgentThatExitsDuringAPrompt(t *testing.T) {
	var first atomic.Int64
	held := func() bool {
		now := time.Now().UnixNano()
		first.CompareAndSwap(0, now)
		return time.Duration(now-first.Load()) < 50*time.Millisecond
	}
	p := startExitingPrompt(t, t.Context(), held)
	took, err := p.exit(t)
	if err == nil || errors.Is(err, ErrStopUnconfirmed) || !PromptSettled(err) {
		t.Fatalf("prompt of an agent whose group emptied returned %v, want an ordinary failure", err)
	}
	if !p.h.ProcessStopped(p.generation) {
		t.Fatal("the stop of an agent whose group emptied was not confirmed")
	}
	if took >= exitedGroupWait {
		t.Fatalf("the prompt returned %v after the agent exited, though its group emptied within milliseconds", took.Round(time.Millisecond))
	}
}

// A member of the group that outlasts the wait keeps the stop unconfirmed:
// the prompt waits for the group only briefly, and does not claim a stop it
// did not see. The stop is confirmed once the group is empty.
func TestHostWaitsOnlyBrieflyForTheGroupOfAnAgentThatExitsDuringAPrompt(t *testing.T) {
	var held atomic.Bool
	held.Store(true)
	p := startExitingPrompt(t, t.Context(), held.Load)
	t.Cleanup(func() { held.Store(false) })
	took, err := p.exit(t)
	if !errors.Is(err, ErrStopUnconfirmed) || PromptSettled(err) {
		t.Fatalf("prompt of an agent whose group still runs returned %v, want %v", err, ErrStopUnconfirmed)
	}
	if took > exitedGroupWait+time.Second {
		t.Fatalf("the prompt waited %v for a group that did not empty", took.Round(time.Millisecond))
	}
	if p.h.ProcessStopped(p.generation) {
		t.Fatal("the stop was confirmed while a member of the agent's group still ran")
	}
	held.Store(false)
	for deadline := time.Now().Add(10 * time.Second); !p.h.ProcessStopped(p.generation); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stop was not confirmed once the agent's group was empty")
		}
	}
}

// A prompt its caller has given up on returns as soon as the agent exits:
// it does not wait for the group, and leaves the stop unconfirmed while a
// member runs.
func TestHostDoesNotWaitForTheGroupOfAnAgentThatExitsDuringACancelledPrompt(t *testing.T) {
	var held atomic.Bool
	held.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := startExitingPrompt(t, ctx, held.Load)
	t.Cleanup(func() { held.Store(false) })
	cancel()
	took, err := p.exit(t)
	if !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("cancelled prompt of an agent whose group still runs returned %v, want %v", err, ErrStopUnconfirmed)
	}
	if took >= exitedGroupWait/2 {
		t.Fatalf("the cancelled prompt waited %v for the agent's group", took.Round(time.Millisecond))
	}
}
