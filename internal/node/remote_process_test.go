package node

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

func memoryRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry("hub", map[string]Config{"n": {}})
	t.Cleanup(r.Close)
	return r
}

func connectMemory(t *testing.T, m *memoryNode, r *Registry) *conn {
	t.Helper()
	c := &conn{name: "n", mux: m.connection(t), advert: nodewire.Advert{Features: nodewire.Features(),
		SessionGraceMS: m.s.sessionGrace().Milliseconds(), Harnesses: []nodewire.Harness{{ID: "cat"}}}}
	r.eventMu.Lock()
	r.mu.Lock()
	if r.gens == nil {
		r.gens = map[string]int64{}
	}
	r.gens["n"]++
	c.generation = r.gens["n"]
	r.live["n"] = c
	r.signalLocked("n")
	r.clocksLocked("n", true)
	r.mu.Unlock()
	r.eventMu.Unlock()
	go func() { <-c.mux.Done(); r.down(c) }()
	return c
}

func startRemote(t *testing.T, r *Registry) *remoteProcess {
	t.Helper()
	p, err := r.Transport("n", "cat").Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Kill)
	return p.(*remoteProcess)
}

func TestDisconnectedProcessWaitDoesNotProvePhysicalStop(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	p := startRemote(t, r)
	_, _ = io.WriteString(p.Stdin(), "ready\n")
	expectLine(t, bufio.NewReader(p.Stdout()), "ready\n")
	_ = c.mux.Close()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if p.Stopped() {
		t.Fatal("logical disconnect was accepted as physical exit evidence")
	}
}

type lineResult struct {
	line string
	err  error
}

func nextLine(r *bufio.Reader) <-chan lineResult {
	ch := make(chan lineResult, 1)
	go func() { s, err := r.ReadString('\n'); ch <- lineResult{s, err} }()
	return ch
}

func TestRemoteProcessHidesOutageAndBuffersInput(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	p := startRemote(t, r)
	rd := bufio.NewReader(p.Stdout())
	_, _ = io.WriteString(p.Stdin(), "before\n")
	expectLine(t, rd, "before\n")
	_ = c.mux.Close()
	pending := nextLine(rd)
	select {
	case got := <-pending:
		t.Fatalf("EOF during outage: %+v", got)
	case <-time.After(40 * time.Millisecond):
	}
	if _, err := io.WriteString(p.Stdin(), "during\n"); err != nil {
		t.Fatal(err)
	}
	connectMemory(t, m, r)
	select {
	case got := <-pending:
		if got.err != nil || got.line != "during\n" {
			t.Fatalf("resumed: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("did not resume")
	}
	_, _ = io.WriteString(p.Stdin(), "after\n")
	expectLine(t, rd, "after\n")
}

func TestRemoteDiscardsPartialOutputAndReplaysWholeLine(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	m.dropData = 2
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	p := startRemote(t, r)
	line := strings.Repeat("x", nodewire.MaxPayload+100) + "\n"
	nodeProcess := m.process(t, p.id)
	nodeProcess.emit(outputLine{data: []byte(line)}, true)
	pending := nextLine(bufio.NewReader(p.Stdout()))
	<-c.mux.Done()
	select {
	case got := <-pending:
		t.Fatalf("partial output escaped: len=%d err=%v", len(got.line), got.err)
	case <-time.After(20 * time.Millisecond):
	}
	p.mu.Lock()
	out := p.out
	p.mu.Unlock()
	if out != 0 {
		t.Fatalf("counted a partial line: %d", out)
	}
	m.dropData = 0
	connectMemory(t, m, r)
	select {
	case got := <-pending:
		if got.err != nil || got.line != line {
			t.Fatalf("resumed len=%d err=%v", len(got.line), got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
}

func TestRemoteCloseDuringOutageReleasesOnReconnect(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	p := startRemote(t, r)
	nodeProcess := m.process(t, p.id)
	_ = c.mux.Close()
	_ = p.Close()
	connectMemory(t, m, r)
	waitFor(t, func() bool {
		nodeProcess.mu.Lock()
		defer nodeProcess.mu.Unlock()
		return nodeProcess.released && nodeProcess.exit != ""
	})
}

func TestRemoteGraceAndExplicitRelease(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "grace", true: "release"}[explicit], func(t *testing.T) {
			m := newMemoryNode(t, "/bin/cat")
			cfg := m.s.conf()
			cfg.SessionGrace = 120 * time.Millisecond
			m.s.cfg.Store(&cfg)
			r := memoryRegistry(t)
			c := connectMemory(t, m, r)
			p := startRemote(t, r)
			read := nextLine(bufio.NewReader(p.Stdout()))
			start := time.Now()
			_ = c.mux.Close()
			if explicit {
				r.Remove("n")
			}
			select {
			case got := <-read:
				if got.err == nil {
					t.Fatal("missing terminal error")
				}
				if !explicit && time.Since(start) < 100*time.Millisecond {
					t.Fatalf("EOF before grace: %v", got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("terminal error never surfaced")
			}
		})
	}
}

func TestRemoteInputBufferLimitAndAnsweredRequestReplay(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	p := startRemote(t, r)
	nodeProcess := m.process(t, p.id)
	rd := bufio.NewReader(p.Stdout())
	request := "{\"jsonrpc\":\"2.0\",\"id\":\"permission-1\",\"method\":\"session/request_permission\",\"params\":{}}\n"
	nodeProcess.emit(outputLine{data: []byte(request)}, true)
	expectLine(t, rd, request)
	answer := "{\"jsonrpc\":\"2.0\",\"id\":\"permission-1\",\"result\":{}}\n"
	_, _ = io.WriteString(p, answer)
	expectLine(t, rd, answer)
	_ = c.mux.Close()
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return !p.ready })
	if _, err := p.Write(bytes.Repeat([]byte("x"), inputBufferBytes+1)); !errors.Is(err, errInputBufferFull) {
		t.Fatal(err)
	}
	nodeProcess.emit(outputLine{data: []byte(request)}, true)
	nodeProcess.emit(outputLine{data: []byte("end\n")}, true)
	connectMemory(t, m, r)
	expectLine(t, rd, "end\n")
	var input bytes.Buffer
	if err := nodeProcess.journal.In.Replay(0, &input); err != nil {
		t.Fatal(err)
	}
	if input.String() != answer {
		t.Fatalf("repeated permission answer: %q", input.String())
	}
}

func TestRemoteResendsOnlyInputsAboveResumeAck(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	m.dropAcks = true
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	p := startRemote(t, r)
	rd := bufio.NewReader(p.Stdout())
	_, _ = io.WriteString(p, "executed\n")
	expectLine(t, rd, "executed\n")
	_ = c.mux.Close()
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return !p.ready })
	p.mu.Lock()
	if p.haveIn != 0 || len(p.queue) != 1 {
		p.mu.Unlock()
		t.Fatal("expected an executed but unacknowledged input")
	}
	p.mu.Unlock()
	m.dropAcks = false
	_, _ = io.WriteString(p, "new\n")
	connectMemory(t, m, r)
	expectLine(t, rd, "new\n")
	nodeProcess := m.process(t, p.id)
	var input bytes.Buffer
	if err := nodeProcess.journal.In.Replay(0, &input); err != nil {
		t.Fatal(err)
	}
	if input.String() != "executed\nnew\n" {
		t.Fatalf("inputs were executed twice: %q", input.String())
	}
}

func TestSameHarnessConcurrentSessionsSurviveReconnectAndOneCancellation(t *testing.T) {
	m := newMemoryNode(t, buildMockAgent(t))
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	broker, _ := permission.New("auto")
	host := acphost.New(acphost.Config{Transport: r.Transport("n", "cat"), Permission: broker})
	t.Cleanup(host.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a, generation, err := host.OpenSession(ctx, "", acphost.SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b, g2, err := host.OpenSession(ctx, "", acphost.SessionConfig{Workdir: t.TempDir()})
	if err != nil || g2 != generation {
		t.Fatal(err, generation, g2)
	}
	var p *agentProcess
	waitFor(t, func() bool {
		m.s.processMu.Lock()
		defer m.s.processMu.Unlock()
		for _, process := range m.s.processes {
			p = process
		}
		return p != nil
	})
	childCtx, stopChild := context.WithCancel(ctx)
	first := make(chan error, 1)
	go func() { _, _, err := host.Prompt(childCtx, a, generation, "slow work", nil); first <- err }()
	permissionAsked, answer := make(chan struct{}), make(chan struct{})
	second := make(chan error, 1)
	go func() {
		_, _, err := host.PromptTurn(ctx, b, generation, "askme", nil, nil, func(ctx context.Context, _ view.Question) (view.Answer, error) {
			close(permissionAsked)
			select {
			case <-answer:
			case <-ctx.Done():
				return view.Answer{}, ctx.Err()
			}
			return view.Answer{Value: "Red"}, nil
		}, nil)
		second <- err
	}()
	<-permissionAsked
	// Wait until both prompt lines reached the one shared process.
	time.Sleep(20 * time.Millisecond)
	_ = c.mux.Close()
	stopChild()
	connectMemory(t, m, r)
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(answer)
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	out, _, err := host.Prompt(ctx, b, generation, "still alive", nil)
	if err != nil || !strings.Contains(out, "echo: still alive") {
		t.Fatal(out, err)
	}
	m.s.processMu.Lock()
	count := len(m.s.processes)
	m.s.processMu.Unlock()
	if count != 1 {
		t.Fatalf("started %d harness processes", count)
	}
}

type trackedClock struct {
	mu              sync.Mutex
	paused          bool
	pauses, resumes int
}

func (c *trackedClock) Pause()  { c.mu.Lock(); defer c.mu.Unlock(); c.paused = true; c.pauses++ }
func (c *trackedClock) Resume() { c.mu.Lock(); defer c.mu.Unlock(); c.paused = false; c.resumes++ }

func TestLateDownCannotPauseNewConnectionOrEmitOldEvent(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	r := memoryRegistry(t)
	old := connectMemory(t, m, r)
	clock := &trackedClock{}
	unregister := r.RegisterIdle("n", clock)
	defer unregister()
	var mu sync.Mutex
	var events []Status
	r.SetObserver(func(s Status) { mu.Lock(); defer mu.Unlock(); events = append(events, s) })
	_ = old.mux.Close()
	r.down(old)
	newConn := connectMemory(t, m, r)
	r.down(old)
	clock.mu.Lock()
	paused, pauses, resumes := clock.paused, clock.pauses, clock.resumes
	clock.mu.Unlock()
	if paused || pauses != 1 || resumes != 1 {
		t.Fatalf("clock paused=%v calls=%d/%d", paused, pauses, resumes)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Generation != old.generation || events[0].Up {
		t.Fatalf("events=%+v", events)
	}
	if newConn.generation <= old.generation {
		t.Fatal("generation did not advance")
	}
}

func TestRegisteredIdleClockSurvivesOutageButHardBudgetDoesNot(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	parent, cancel := context.WithTimeout(t.Context(), 160*time.Millisecond)
	defer cancel()
	clock, stop, _ := idle.WithTimeout(parent, 60*time.Millisecond)
	defer stop()
	unregister := r.RegisterIdle("n", clock)
	defer unregister()
	_ = c.mux.Close()
	r.down(c)
	time.Sleep(80 * time.Millisecond)
	if clock.Err() != nil {
		t.Fatal("silence ran during outage", clock.Err())
	}
	<-clock.Done()
	if !errors.Is(clock.Err(), context.DeadlineExceeded) {
		t.Fatal(clock.Err())
	}
}
