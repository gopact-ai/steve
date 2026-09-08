package turn

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/state"
)

func TestSelectedLocalTurnDoesNotDialUnrelatedNodes(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{"local": {Harness: "mock", Default: true, Requires: []string{"chat"}}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{reply: "hello"}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), &fakeManager{runners: map[string]*fakeRunner{"mock": runner}}, time.Minute)
	var dials atomic.Int32
	registry := node.NewRegistry("test", map[string]node.Config{
		"offline": {DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
			dials.Add(1)
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
			}
			return nil, errors.New("test node offline")
		}},
	})
	defer registry.Close()
	c.fleet = roster.New(catalog)
	c.fleet.SetNodes(registry)
	c.fleet.SetHubCapabilities([]string{"chat"})
	started := time.Now()
	result, err := handle(c, t.Context(), "hello")
	t.Logf("selected local turn elapsed=%s", time.Since(started))
	if err != nil || result.Text != "hello" {
		t.Fatalf("turn = %+v, %v", result, err)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("local turn dialed unrelated offline node %d times", got)
	}
	record, err := c.attempts.Get(t.Context(), result.Attempt)
	if err != nil || record.Admission == nil || !record.Admission.OK() {
		t.Fatalf("selected turn lost admission: %+v, %v", record, err)
	}
}
