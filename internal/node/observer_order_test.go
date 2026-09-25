package node

import (
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// observerGate makes one observer call wait until the test lets it go,
// the way an observer waiting on a replicated write would.
type observerGate struct {
	entered, release chan struct{}
	once             sync.Once
}

func newObserverGate(t *testing.T) *observerGate {
	g := &observerGate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *observerGate) hold() {
	close(g.entered)
	<-g.release
}

func (g *observerGate) open() { g.once.Do(func() { close(g.release) }) }

func (g *observerGate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the observer was never called")
	}
}

// promptly runs what the registry does for a connection change and fails
// if it is still waiting once the bound is past. With the observer held,
// only a registry that makes connection handling wait for it gets there.
func promptly(t *testing.T, what string, change func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- change() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s waited for an observer still hearing an earlier change", what)
	}
}

// heardAll returns once every change posted so far has been delivered:
// notices are delivered in order, so a marker posted now arrives last.
func heardAll(t *testing.T, r *Registry) {
	t.Helper()
	delivered := make(chan struct{})
	r.notices.post(func() { close(delivered) })
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("posted changes were never delivered")
	}
}

func liveConn(t *testing.T, r *Registry, name string) *conn {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.live[name]
	if c == nil {
		t.Fatalf("%s is not connected", name)
	}
	return c
}

func disconnect(r *Registry, c *conn) error {
	_ = c.mux.Close()
	r.down(c)
	return nil
}

// Observers write history, which can wait on a replicated write for as long
// as the ledger takes. While one is still taking in a machine's connection,
// other machines connect and drop, and that machine drops too, without
// waiting for it; the observer still hears each machine's changes in the
// order they happened, a connection before its loss.
func TestSlowObserverDelaysNoConnectionChange(t *testing.T) {
	alpha := startNode(t, ServerConfig{Name: "alpha", Token: "tok", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir()})
	beta := startNode(t, ServerConfig{Name: "beta", Token: "tok", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir()})
	r := NewRegistry("hub-1", map[string]Config{
		"alpha": {Addr: alpha.Addr(), Token: "tok"},
		"beta":  {Addr: beta.Addr(), Token: "tok"},
	})
	t.Cleanup(r.Close)
	gate := newObserverGate(t)
	heard := make(chan Status, 16)
	r.SetObserver(func(s Status) {
		if s.Name == "alpha" && s.Up {
			gate.hold()
		}
		heard <- s
	})
	go func() { _, _ = r.Advert(t.Context(), "alpha") }()
	gate.waitEntered(t)

	promptly(t, "connecting beta", func() error { _, err := r.Advert(t.Context(), "beta"); return err })
	promptly(t, "disconnecting beta", func() error { return disconnect(r, liveConn(t, r, "beta")) })
	promptly(t, "disconnecting alpha", func() error { return disconnect(r, liveConn(t, r, "alpha")) })

	gate.open()
	order := map[string][]bool{}
	for range 4 {
		select {
		case s := <-heard:
			order[s.Name] = append(order[s.Name], s.Up)
		case <-time.After(10 * time.Second):
			t.Fatalf("heard only %v", order)
		}
	}
	for _, name := range []string{"alpha", "beta"} {
		if got := order[name]; len(got) != 2 || !got[0] || got[1] {
			t.Errorf("%s heard as up=%v, want its connection and then its loss", name, got)
		}
	}
}

// Manifest drift is history too, reported on the way to taking a new
// advert; a slow drift observer does not hold up the connection or refresh
// that found the change.
func TestSlowDriftObserverDelaysNoAdvert(t *testing.T) {
	r := NewRegistry("hub-1", map[string]Config{"n": {}})
	t.Cleanup(r.Close)
	r.mu.Lock()
	r.last["n"] = &Status{Name: "n", Up: true, Advert: nodewire.Advert{Node: "n", Capabilities: []string{"gpu"}}}
	r.mu.Unlock()
	gate := newObserverGate(t)
	heard := make(chan string, 1)
	r.SetDriftObserver(func(name string, _ []ability.Change) {
		gate.hold()
		heard <- name
	})
	promptly(t, "taking a changed advert", func() error {
		r.noteDrift("n", nodewire.Advert{Node: "n", Capabilities: []string{"gpu", "fpga"}})
		return nil
	})
	gate.waitEntered(t)
	gate.open()
	select {
	case name := <-heard:
		if name != "n" {
			t.Fatalf("drift heard for %q", name)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("drift was never heard")
	}
}

// A closed registry reports nothing more: what was still queued behind a
// slow observer is dropped rather than written after the hub let go.
func TestClosedRegistryDeliversNothingStillQueued(t *testing.T) {
	r := NewRegistry("hub-1", map[string]Config{"n": {}})
	gate := newObserverGate(t)
	var mu sync.Mutex
	var heard []bool
	r.SetObserver(func(s Status) {
		if s.Up {
			gate.hold()
		}
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, s.Up)
	})
	r.mu.Lock()
	r.noticeLocked(Status{Name: "n", Up: true})
	r.noticeLocked(Status{Name: "n"})
	r.mu.Unlock()
	gate.waitEntered(t)
	r.Close()
	gate.open()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.notices.mu.Lock()
		running := r.notices.running
		r.notices.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the delivery under way never finished")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 1 || !heard[0] {
		t.Fatalf("heard up=%v after close, want only the delivery already under way", heard)
	}
}
