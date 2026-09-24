package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/roster"
)

// errorSignal reports once an error record whose message holds text is logged.
type errorSignal struct {
	slog.Handler
	text string
	once sync.Once
	seen chan struct{}
}

func (h *errorSignal) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && strings.Contains(r.Message, h.text) {
		h.once.Do(func() { close(h.seen) })
	}
	return h.Handler.Handle(ctx, r)
}

// The model probe and the lease issuer keep working while the
// administration rewrites the configuration they were built from.
func TestModelProbesReadTheirConfigurationWhileItIsRewritten(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	signal := &errorSignal{Handler: slog.NewTextHandler(io.Discard, nil), text: "lease issuer", seen: make(chan struct{})}
	previous := slog.Default()
	slog.SetDefault(slog.New(signal))
	t.Cleanup(func() { slog.SetDefault(previous) })

	state := t.TempDir()
	cfg := &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(state, "state.json"), IssuerAddr: taken.Addr().String()}}
	book, err := ledger.Open(state, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes := node.NewRegistry("hub", nil)
	t.Cleanup(nodes.Close)
	store := adminsvc.NewConfigStore(cfg)
	life := &applicationLifetime{}
	t.Cleanup(func() { life.Close() })
	boot := &runtimeValues{book: book, cfg: cfg, configStore: store, manager: manager, catalog: catalog}
	assembled, err := assembleModels(life, boot, &fleetValues{fleet: roster.New(catalog), nodes: nodes})
	if err != nil {
		t.Fatal(err)
	}

	rewrite := func() {
		if err := store.Update(func(c *config.Config) error {
			c.Gateway.HubID += "x"
			return nil
		}, func(*config.Config) error { return nil }); err != nil {
			t.Error(err)
		}
	}
	probed := make(chan string)
	go func() { probed <- assembled.ProbeDir()("") }()
	rewrite()
	if dir := <-probed; dir != filepath.Join(state, "probe") {
		t.Fatalf("probe directory = %q", dir)
	}
	<-signal.seen
	rewrite()
}
