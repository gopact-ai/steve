package platformconfig

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
)

func initialConfig() *config.Config {
	return &config.Config{
		Gateway:    config.Gateway{OwnerID: "owner", HomePath: "/local/home", StatePath: "/local/state.json", ReadModelToken: "ui-secret", DefaultProject: "work"},
		Harnesses:  map[string]config.Harness{"codex": {Command: "/local/bin/adapter", Env: []string{"KEY=agent-secret"}}},
		MCPServers: map[string]config.MCPServer{"private": {URL: "http://private", Headers: map[string]string{"Authorization": "mcp-secret"}}},
		Agents:     map[string]config.Agent{"main": {Harness: "codex", Default: true}},
		Projects:   map[string]config.Project{"work": {Home: config.ProjectHome{Path: "/local/work"}}},
	}
}

func TestSharedConfigKeepsPhysicalLocationsAndExcludesLocalSecrets(t *testing.T) {
	cfg := initialConfig()
	d, err := FromLocal(cfg, LocalNode{ID: "machine-a", Config: config.Node{Addr: "worker-a", Token: "node-access"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Agents["main"].Node != "machine-a" || d.Projects["work"].Home.Node != "machine-a" || d.Home.Node != "machine-a" {
		t.Fatalf("locations = %+v", d)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ui-secret", "agent-secret", "mcp-secret", "/local/state.json", "/local/bin/adapter"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("shared declaration included local setting %q", secret)
		}
	}
	other := &config.Config{Gateway: config.Gateway{StatePath: "/other/state.json", HomePath: "/other/home", ReadModelToken: "other-token"}}
	if err := d.Apply(other); err != nil {
		t.Fatal(err)
	}
	if other.Gateway.HomePath != "/other/home" || other.Gateway.ReadModelToken != "other-token" || other.RuntimeHome.Node != "machine-a" || other.Agents["main"].Node != "machine-a" {
		t.Fatalf("role change moved machine-local state: %+v", other)
	}
	a := other.Agents["main"]
	a.Node = "other"
	other.Agents["main"] = a
	if d.Agents["main"].Node != "machine-a" {
		t.Fatal("application aliased shared snapshot")
	}
}

func TestSharedConfigCASRejectsStaleDeclaration(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	store := New(book)
	first, err := store.Bootstrap(t.Context(), initialConfig(), LocalNode{ID: "machine-a", Config: config.Node{Addr: "worker-a", Token: "node-access"}})
	if err != nil {
		t.Fatal(err)
	}
	next := first
	next.Settings.Gateway.Locale = "en"
	committed, err := store.Save(t.Context(), first.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(t.Context(), first.Revision, first); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write = %v", err)
	}
	got, ok, err := store.Load()
	if err != nil || !ok || got.Revision != committed.Revision || got.Settings.Gateway.Locale != "en" {
		t.Fatalf("load = %+v %v %v", got, ok, err)
	}
	first.Agents["main"] = config.Agent{Harness: "codex", Default: true}
	if _, err := store.Save(t.Context(), got.Revision, first); err == nil {
		t.Fatal("accepted a role-relative agent location")
	}
}
