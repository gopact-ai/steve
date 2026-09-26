package agentmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/memory"
)

// closingStore stands in for the storage a tool call writes through. Its
// owner closes it as soon as Start returns, the way the application closes
// the ledger once the server's goroutine is gone; a write after that is
// lost.
type closingStore struct {
	mu     sync.Mutex
	closed bool
	writes int
	lost   int
}

func (c *closingStore) write() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.lost++
		return
	}
	c.writes++
}

func (c *closingStore) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *closingStore) counts() (writes, lost int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes, c.lost
}

// heldMemorizer holds steve_remember until it is released or its request
// ends, then writes through the store either way, as a tool that records
// its outcome does.
type heldMemorizer struct {
	Memorizer
	store   *closingStore
	entered chan struct{}
	release chan struct{}
}

func newHeldMemorizer(store *closingStore) *heldMemorizer {
	return &heldMemorizer{store: store, entered: make(chan struct{}), release: make(chan struct{})}
}

func (m *heldMemorizer) Remember(ctx context.Context, _, _, _, _, _, _, _ string) (memory.Receipt, memory.Scope, error) {
	close(m.entered)
	select {
	case <-m.release:
	case <-ctx.Done():
	}
	m.store.write()
	return memory.Receipt{ID: "g1", New: true}, memory.Global, nil
}

// postToolCall issues one tools/call without the test's Fatal helpers, so it
// can run on its own goroutine.
func postToolCall(url, token, name string, args map[string]any) error {
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tools/call status %d", resp.StatusCode)
	}
	return nil
}

// serveUntilStoreCloses runs Start and closes store the moment it returns.
func serveUntilStoreCloses(t *testing.T, s *Server, store *closingStore) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() {
		err := s.Start(ctx)
		store.close()
		stopped <- err
	}()
	t.Cleanup(cancel)
	return cancel, stopped
}

func waitEntered(t *testing.T, m *heldMemorizer) {
	t.Helper()
	select {
	case <-m.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("tool call never reached the memorizer")
	}
}

// A tool call that is still running when the server is asked to stop
// finishes, and writes, before Start returns: whoever owns the storage
// closes it on Start's return.
func TestStartReturnsOnlyAfterInFlightToolCallsFinish(t *testing.T) {
	s, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	store := &closingStore{}
	m := newHeldMemorizer(store)
	s.SetMemorizer(m)
	register(s, "oc_a", "builder", "tok-a", "om_1")
	cancel, stopped := serveUntilStoreCloses(t, s, store)
	answered := make(chan error, 1)
	go func() {
		answered <- postToolCall(s.URL(), "tok-a", "steve_remember", map[string]any{"scope": "global", "text": "likes go"})
	}()
	waitEntered(t, m)

	cancel()
	returnedEarly := false
	select {
	case err := <-stopped:
		returnedEarly = true
		if err != nil {
			t.Errorf("Start: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
	}
	close(m.release)
	if err := <-answered; err != nil {
		t.Errorf("in-flight tool call: %v", err)
	}
	if !returnedEarly {
		select {
		case err := <-stopped:
			if err != nil {
				t.Errorf("Start: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Start did not return after the in-flight call finished")
		}
	}
	writes, lost := store.counts()
	if returnedEarly || lost != 0 || writes != 1 {
		t.Fatalf("Start returned while a tool call was running (early=%v): %d write(s) kept, %d written after the store closed", returnedEarly, writes, lost)
	}
}

// A tool call that would outlast the grace period has its request
// cancelled, and Start still waits for it to return before it does.
func TestStartCancelsToolCallsThatOutlastTheGracePeriod(t *testing.T) {
	s, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	s.grace = 50 * time.Millisecond
	store := &closingStore{}
	m := newHeldMemorizer(store)
	t.Cleanup(func() {
		select {
		case <-m.release:
		default:
			close(m.release)
		}
	})
	s.SetMemorizer(m)
	register(s, "oc_a", "builder", "tok-a", "om_1")
	cancel, stopped := serveUntilStoreCloses(t, s, store)
	answered := make(chan error, 1)
	go func() {
		answered <- postToolCall(s.URL(), "tok-a", "steve_remember", map[string]any{"scope": "global", "text": "likes go"})
	}()
	waitEntered(t, m)

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after cancelling the stuck call")
	}
	if writes, lost := store.counts(); writes != 1 || lost != 0 {
		t.Fatalf("stuck call: %d write(s) kept, %d written after the store closed", writes, lost)
	}
	// The cut connection is the caller's to see; it only must not hang.
	select {
	case <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("caller of the cancelled call never got an answer")
	}
}

// New binds its port before anything else is assembled. When assembly
// fails after that, the server is never started, and Close is what gives
// the port back.
func TestCloseReleasesThePortOfAServerNeverStarted(t *testing.T) {
	s, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	port := s.Port()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d still held after Close: %v", port, err)
	}
	l.Close()
}

// Close on a running server shuts it down the way a cancelled Start
// does: it returns once the call in flight has finished, and Start
// returns nil after it.
func TestCloseDrainsARunningServer(t *testing.T) {
	s, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	store := &closingStore{}
	m := newHeldMemorizer(store)
	s.SetMemorizer(m)
	register(s, "oc_a", "builder", "tok-a", "om_1")
	_, stopped := serveUntilStoreCloses(t, s, store)
	answered := make(chan error, 1)
	go func() {
		answered <- postToolCall(s.URL(), "tok-a", "steve_remember", map[string]any{"scope": "global", "text": "likes go"})
	}()
	waitEntered(t, m)

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		close(m.release)
		t.Fatalf("Close returned while a tool call was running: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(m.release)
	if err := <-answered; err != nil {
		t.Errorf("in-flight tool call: %v", err)
	}
	if err := <-closed; err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Start after Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after Close")
	}
	if writes, lost := store.counts(); writes != 1 || lost != 0 {
		t.Fatalf("%d write(s) kept, %d written after the store closed", writes, lost)
	}
}
