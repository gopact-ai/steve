package app

import (
	"path/filepath"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/roster"
)

// The model probe keeps working while the administration rewrites the
// configuration it was built from.
func TestModelProbesReadTheirConfigurationWhileItIsRewritten(t *testing.T) {
	state := t.TempDir()
	cfg := &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(state, "state.json")}}
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
	rewrite()
}
