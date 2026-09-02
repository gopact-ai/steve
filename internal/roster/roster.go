// Package roster answers "who can do this, and where" from live facts.
//
// The authority on what a machine can run is that machine's advert, taken on
// a real connection — not a line in the hub's config. A config that claims a
// node has a GPU does not put one there, and placing work on the strength of
// that claim fails later, further from the cause.
package roster

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// NodeSource is the live view of remote machines. The node registry
// satisfies it; a hub with no nodes passes nil.
//
// EnsureConnected is part of the contract because a placement decision is
// exactly the moment worth paying a dial for: reporting a node as down
// because nobody has tried it yet would describe the registry's ignorance
// rather than the fleet.
type NodeSource interface {
	Statuses() []node.Status
	EnsureConnected(ctx context.Context)
}

// Candidate is one agent considered for a placement, with the reason it does
// or does not qualify. A roster that only lists winners cannot explain a
// refusal, and "nothing matched" is the message people actually need.
type Candidate struct {
	Agent    agent.Agent
	Node     string
	Up       bool
	Eligible bool
	// Why explains an ineligible candidate: the node is down, the harness
	// is missing there, a required capability is absent.
	Why          string
	Capabilities []string
	Models       []string
	Harness      string
}

type Roster struct {
	catalog *agent.Catalog

	mu sync.RWMutex
	// hubCaps are what the hub's own machine offers. Hub-local agents are
	// not exempt from capability matching: an agent that needs a GPU should
	// not silently run here just because here is the default.
	hubCaps []string
	nodes   NodeSource
}

func New(catalog *agent.Catalog) *Roster { return &Roster{catalog: catalog} }

func (r *Roster) SetNodes(nodes NodeSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = nodes
}

// SetHubCapabilities declares what the hub's own machine can do.
func (r *Roster) SetHubCapabilities(caps []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hubCaps = append([]string(nil), caps...)
}

// All reports every configured agent with its current standing. This is what
// /status, the read model and `doctor` render.
func (r *Roster) All(ctx context.Context) []Candidate {
	r.mu.RLock()
	nodes, hubCaps := r.nodes, append([]string(nil), r.hubCaps...)
	r.mu.RUnlock()

	byNode := map[string]node.Status{}
	if nodes != nil {
		nodes.EnsureConnected(ctx)
		for _, s := range nodes.Statuses() {
			byNode[s.Name] = s
		}
	}

	out := make([]Candidate, 0, len(r.catalog.List()))
	for _, a := range r.catalog.List() {
		out = append(out, describe(a, byNode, hubCaps))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent.ID < out[j].Agent.ID })
	return out
}

// Candidates lists agents that can run a step needing these capabilities,
// best first: reachable agents before unreachable ones, and among equals the
// stable alphabetical order so a plan re-run lands the same way.
func (r *Roster) Candidates(ctx context.Context, requires []string, exclude []string) []Candidate {
	skip := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		skip[id] = true
	}
	var out []Candidate
	for _, c := range r.All(ctx) {
		if skip[c.Agent.ID] {
			continue
		}
		if !c.Eligible {
			continue
		}
		if missing := missingCaps(requires, c.Capabilities); missing != "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Explain says why nothing matched, naming each agent's disqualifying
// reason. A placement failure that only says "no agent available" sends
// someone reading config files instead of restarting a node.
func (r *Roster) Explain(ctx context.Context, requires []string) string {
	var reasons []string
	for _, c := range r.All(ctx) {
		switch {
		case !c.Eligible:
			reasons = append(reasons, c.Agent.ID+": "+c.Why)
		default:
			if missing := missingCaps(requires, c.Capabilities); missing != "" {
				reasons = append(reasons, c.Agent.ID+": lacks "+missing)
			}
		}
	}
	if len(reasons) == 0 {
		return "no agents are configured"
	}
	return strings.Join(reasons, "; ")
}

func describe(a agent.Agent, byNode map[string]node.Status, hubCaps []string) Candidate {
	c := Candidate{Agent: a, Node: a.Node, Harness: a.Harness, Eligible: true}
	if a.Model != "" {
		c.Models = []string{a.Model}
	}
	if a.Node == "" {
		c.Up = true
		c.Capabilities = hubCaps
	} else {
		status, known := byNode[a.Node]
		if !known {
			c.Eligible, c.Why = false, "node "+a.Node+" is not configured"
			return c
		}
		c.Up = status.Up
		c.Capabilities = status.Advert.Capabilities
		if !status.Up {
			c.Eligible = false
			c.Why = "node " + a.Node + " is down"
			if status.LastError != "" {
				c.Why += ": " + status.LastError
			}
			return c
		}
		if missing := harnessTrouble(status.Advert, a.Harness); missing != "" {
			c.Eligible, c.Why = false, missing
			return c
		}
		offered := harnessModels(status.Advert, a.Harness)
		if len(offered) > 0 {
			c.Models = offered
			// A pinned model the node does not offer is a placement that
			// would fail at the first prompt; catch it here, where the
			// cause is still legible.
			if a.Model != "" && !contains(offered, a.Model) {
				c.Eligible = false
				c.Why = "node " + a.Node + " does not offer model " + a.Model
				return c
			}
		}
	}
	// An agent's own requirements gate it wherever it runs, the hub included.
	if missing := missingCaps(a.Requires, c.Capabilities); missing != "" {
		c.Eligible = false
		c.Why = "lacks " + missing
	}
	return c
}

func harnessModels(advert nodewire.Advert, harnessID string) []string {
	for _, h := range advert.Harnesses {
		if h.ID == harnessID {
			return h.Models
		}
	}
	return nil
}

func harnessTrouble(advert nodewire.Advert, harnessID string) string {
	for _, h := range advert.Harnesses {
		if h.ID == harnessID {
			if h.Missing != "" {
				return h.Missing
			}
			return ""
		}
	}
	return "node " + advert.Node + " does not offer harness " + harnessID
}

// missingCaps names the first requirement not met, or "" when all are.
func missingCaps(requires, have []string) string {
	for _, want := range requires {
		if want == "" || want == "any" {
			continue
		}
		if !contains(have, want) {
			return want
		}
	}
	return ""
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
