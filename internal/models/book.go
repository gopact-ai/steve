// Package models remembers what each harness on each machine said about
// its models, so the fleet answers "which model" from observation rather
// than from a line someone typed into a config.
//
// The source is ACP itself: a session's config options carry a model
// selector with the current value and the alternatives. Every opened
// session reports it, and a probe can open one on purpose.
package models

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// Observation is one harness on one machine as last seen running. Node ""
// is the hub's own machine, as everywhere in the model.
type Observation struct {
	Node    string `json:"node,omitempty"`
	Harness string `json:"harness"`
	// Current is the model the harness ran by default; Available are the
	// alternatives it offered, by display name.
	Current   string   `json:"current,omitempty"`
	Available []string `json:"available,omitempty"`
	// Selectors are every option the harness exposed, so an agent can
	// pin reasoning effort or mode as well as the model.
	Selectors []Selector `json:"selectors,omitempty"`
	// Version is the adapter's own name and version, as it introduced
	// itself over ACP.
	Version string `json:"version,omitempty"`
	// Source says how it was learned: "session" (ordinary work) or
	// "probe" (asked on purpose).
	Source string    `json:"source"`
	At     time.Time `json:"at"`
}

// Selector is one option a harness exposes, as last seen.
type Selector struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Category string   `json:"category,omitempty"`
	Current  string   `json:"current,omitempty"`
	Choices  []string `json:"choices,omitempty"`
	Values   []string `json:"values,omitempty"`
}

// Book is the set of observations, kept in a durable document so a restart
// does not forget what every harness can do.
type Book struct {
	mu   sync.Mutex
	seen map[string]Observation
	doc  ledger.Doc
}

func New() *Book { return &Book{seen: map[string]Observation{}} }

// Persist loads what an earlier process observed and keeps writing.
func (b *Book) Persist(doc ledger.Doc) error {
	raw, ok, err := doc.Load()
	if err != nil {
		return fmt.Errorf("models: load: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok && len(raw) > 0 {
		var saved []Observation
		if err := json.Unmarshal(raw, &saved); err != nil {
			return fmt.Errorf("models: document is not readable: %w", err)
		}
		for _, o := range saved {
			b.seen[key(o.Node, o.Harness)] = o
		}
	}
	b.doc = doc
	return nil
}

// Observe records what a harness reported. An observation with nothing in
// it — a harness that exposes no model selector — is still worth keeping:
// it says "asked, and it does not tell".
func (b *Book) Observe(o Observation) {
	if o.At.IsZero() {
		o.At = time.Now().UTC()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen[key(o.Node, o.Harness)] = o
	if b.doc != nil {
		if raw, err := json.Marshal(b.all()); err == nil {
			if err := b.doc.Save(raw); err != nil {
				slog.Error(fmt.Sprintf("models: save: %v", err))
			}
		}
	}
}

// Get is the last observation of a harness on a machine.
func (b *Book) Get(node, harness string) (Observation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.seen[key(node, harness)]
	return o, ok
}

// All lists every observation, in a stable order.
func (b *Book) All() []Observation {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.all()
}

func (b *Book) all() []Observation {
	out := make([]Observation, 0, len(b.seen))
	for _, o := range b.seen {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Harness < out[j].Harness
	})
	return out
}

func key(node, harness string) string { return node + "/" + harness }
