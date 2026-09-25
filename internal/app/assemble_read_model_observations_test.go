package app

import (
	"context"
	"path/filepath"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	steveexec "github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/skills"
)

// The hub keeps each connection fact as its own ledger record, and a hub
// assembled again on the same ledger shows them in its history.
func TestReadModelKeepsObservationsAcrossAssemblies(t *testing.T) {
	root := t.TempDir()
	book, err := ledger.Open(filepath.Join(root, "ledger"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	assemble := func() readModelAssembly {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		background := newApplicationBackground(ctx)
		t.Cleanup(background.Close)
		nodes := node.NewRegistry("test-hub", nil)
		t.Cleanup(nodes.Close)
		m, err := skills.Open(skills.DefaultPath(root))
		if err != nil {
			t.Fatal(err)
		}
		live := &skills.Live{Map: m, Dests: []string{filepath.Join(root, "runtime")}}
		view, err := assembleReadModel(&assemblyInput{},
			&runtimeValues{book: book, cfg: &config.Config{}, ctx: ctx, background: background, live: live},
			&ledgerValues{}, &fleetValues{nodes: nodes, observation: &adminsvc.LocalObservation{}},
			&modelsValues{}, &executionValues{}, &plansValues{stepRunner: &steveexec.AgentRunner{}})
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	assemble().View().Observe("node.up", "alpha", "alpha connected", map[string]string{"build": "v1"})
	assemble().View().Observe("node.down", "alpha", "alpha disconnected", map[string]string{"reason": "eof"})
	records, err := book.Bindings(t.Context(), "observation")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("ledger keeps %d observation records, want one per observation", len(records))
	}
	history, _, err := assemble().View().History(t.Context(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, entry := range history {
		if entry.Subject == "alpha" {
			kinds = append(kinds, entry.Kind)
		}
	}
	if len(kinds) != 2 || kinds[0] != "observe.node.down" || kinds[1] != "observe.node.up" {
		t.Fatalf("restarted history = %v, want both observations newest first", kinds)
	}
}
