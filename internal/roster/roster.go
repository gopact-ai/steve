// Package roster answers "who can do this, and where" from live facts.
//
// The authority on what a machine can run is that machine's advert, taken on
// a real connection — not a line in the hub's config. A config that claims a
// node has a GPU does not put one there, and placing work on the strength of
// that claim fails later, further from the cause.
package roster

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
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
	// Snapshot is what the agent's machine can do — the node's own report
	// plus the models this harness was seen running here — and what a
	// requirement is matched against, scoped to this harness.
	Snapshot *ability.Snapshot
	Models   []string
	Harness  string
	// Model is the model the agent would run with: pinned in config, or
	// the one the harness was last seen running here. Empty means nobody
	// has looked yet. Command is the harness's executable on that machine
	// and Missing why it cannot start there, both from the advert.
	Model string
	// Observed is the model the harness was last seen running here,
	// whatever the agent prefers; Selectors every option it exposed.
	Observed  string
	Selectors []models.Selector
	Command   string
	Missing   string
	// Slots is the endpoint's session cap for (node, harness); zero is
	// unlimited. Level is the data level of the machine.
	Slots int
	Level project.Level
	// Region is whose leases the machine's resources carry ("" = hub's).
	Region string
}

type Roster struct {
	catalog *agent.Catalog

	mu sync.RWMutex
	// hubCaps are what the hub's own machine offers. Hub-local agents are
	// not exempt from capability matching: an agent that needs a GPU should
	// not silently run here just because here is the default.
	hubCaps []string
	// hubLevel and hubSlots describe the hub machine the same way a
	// node's advert and config describe a node.
	hubLevel project.Level
	hubSlots map[string]int
	// hubAdvert is what the hub machine checks about itself, asked fresh
	// each time: a harness whose binary is not on this PATH blocks a
	// hub-local agent exactly as it would on a node, and one installed a
	// minute ago is seen a minute ago.
	hubAdvert func() nodewire.Advert
	// models is what harnesses have been observed running, per machine.
	models     Models
	nodeLevels map[string]project.Level
	regions    map[string]string
	nodes      NodeSource
}

func New(catalog *agent.Catalog) *Roster { return &Roster{catalog: catalog} }

// Admitter is a node source that can ask a machine to re-check a
// requirement on a fresh observation of itself. Sources without it get a
// verdict from the hub's last accepted snapshot, marked as such.
type Admitter interface {
	Admit(ctx context.Context, node string, req nodewire.AdmitRequest) (ability.Admission, error)
	Bindings(ctx context.Context, node, attempt string) []ability.Binding
	Release(ctx context.Context, node, attempt string) error
}

// Release ends an attempt's admission on its machine: the bindings made
// for it go, and the servers behind them stop. The hub's own machine has
// nothing to release.
func (r *Roster) Release(ctx context.Context, node, attemptID string) {
	if node == "" || attemptID == "" {
		return
	}
	r.mu.RLock()
	admitter, ok := r.nodes.(Admitter)
	r.mu.RUnlock()
	if !ok {
		return
	}
	if err := admitter.Release(ctx, node, attemptID); err != nil {
		log.Printf("roster: release %s on %s: %v", attemptID, node, err)
	}
}

// Admit is the final check before an attempt runs on a candidate: the
// hub judges the clauses it owns (models, and everything on its own
// machine) on the candidate's snapshot; the node re-checks the clauses it
// owns on an observation taken now. The first refusal wins; a node that
// cannot be asked leaves the verdict Unsure with its source saying why.
//
// uses names the MCP servers the session will use. Each is a hard
// requirement (mcp:<id>) on top of requires; on a node, each is also bound
// at admission and the launchers come back with the verdict. On the hub's
// own machine the servers are the hub's configuration, so no binding is
// needed.
func (r *Roster) Admit(ctx context.Context, c Candidate, requires []string, uses []string, attemptID string) (ability.Admission, []ability.Binding, error) {
	now := time.Now().UTC()
	all := append([]string(nil), requires...)
	for _, id := range uses {
		all = append(all, "mcp:"+id)
	}
	req, err := ability.Compile(all)
	if err != nil {
		return ability.Admission{}, nil, err
	}
	if c.Node == "" {
		adm := ability.AdmissionOf(c.Match(req), c.Snapshot, ability.SourceHub, now)
		if adm.OK() {
			adm.Bound = append([]string(nil), uses...)
		}
		return adm, nil, nil
	}
	mine, rest := ability.Partition(req, ability.NodeOwned)
	if !rest.Empty() {
		if m := ability.Match(rest, c.Snapshot, c.Harness, now); m.Verdict == ability.False {
			return ability.AdmissionOf(m, c.Snapshot, ability.SourceHub, now), nil, nil
		}
	}
	r.mu.RLock()
	admitter, ok := r.nodes.(Admitter)
	r.mu.RUnlock()
	if !ok || (mine.Empty() && len(uses) == 0) {
		source := ability.SourceCached
		if mine.Empty() {
			source = ability.SourceHub
		}
		return ability.AdmissionOf(ability.Match(req, c.Snapshot, c.Harness, now), c.Snapshot, source, now), nil, nil
	}
	ask := nodewire.AdmitRequest{Attempt: attemptID, Harness: c.Harness, Requirement: mine, Uses: uses}
	if c.Snapshot != nil {
		ask.Generation, ask.Sequence = c.Snapshot.Generation, c.Snapshot.Sequence
	}
	adm, err := admitter.Admit(ctx, c.Node, ask)
	if err != nil || !adm.OK() {
		return adm, nil, err
	}
	return adm, admitter.Bindings(ctx, c.Node, attemptID), nil
}

// ToMCP turns bindings into the MCP servers a session is opened with.
func ToMCP(bindings []ability.Binding) []acp.MCPServer {
	out := make([]acp.MCPServer, 0, len(bindings))
	for _, b := range bindings {
		switch b.Transport {
		case "http":
			out = append(out, acp.HTTPMCPServer(b.Name, b.URL, nil))
		case "sse":
			out = append(out, acp.SSEMCPServer(b.Name, b.URL, nil))
		default:
			out = append(out, acp.StdioMCPServer(b.Name, b.Command, b.Args, nil))
		}
	}
	return out
}

func (r *Roster) SetNodes(nodes NodeSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = nodes
}

// SetHubLevel declares the hub machine's data level.
func (r *Roster) SetHubLevel(level project.Level) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hubLevel = level
}

// SetHubAdvert installs how the hub machine checks itself.
func (r *Roster) SetHubAdvert(adv func() nodewire.Advert) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hubAdvert = adv
}

// Models is the book of observed models; the models package satisfies it.
type Models interface {
	Get(node, harness string) (models.Observation, bool)
}

// SetModels wires the observations in.
func (r *Roster) SetModels(book Models) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = book
}

// SetHubSlots declares the hub's per-harness session caps.
func (r *Roster) SetHubSlots(slots map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hubSlots = slots
}

// SetNodeRegions declares each node's region.
func (r *Roster) SetNodeRegions(regions map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.regions = regions
}

// RegionOf is a node's region; the hub's own is "".
func (r *Roster) RegionOf(node string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.regions[node]
}

// SetNodeLevels declares the level the hub assigned each node.
func (r *Roster) SetNodeLevels(levels map[string]project.Level) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodeLevels = levels
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
	hub, levels, regions := place{level: r.hubLevel, slots: r.hubSlots}, r.nodeLevels, r.regions
	book := r.models
	if r.hubAdvert != nil {
		hub.advert = r.hubAdvert()
	}
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
		c := describe(a, byNode, hubCaps, hub, levels)
		c.Region = regions[a.Node]
		if book != nil {
			if seen, ok := book.Get(a.Node, a.Harness); ok {
				c.Observed = seen.Current
				c.Selectors = seen.Selectors
				if c.Model == "" {
					c.Model = seen.Current
				}
				if len(c.Models) == 0 {
					c.Models = seen.Available
					c.addModels(seen.Available, seen.Version, seen.At)
				}
				// A harness that answered over ACP works, not merely
				// starts: the hub's own evidence, at the hub's own time.
				c.markFunctional(seen.Version, seen.At)
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent.ID < out[j].Agent.ID })
	return out
}

// Candidates lists agents that can run a step needing these capabilities,
// best first: reachable agents before unreachable ones, and among equals the
// stable alphabetical order so a plan re-run lands the same way.
func (r *Roster) Candidates(ctx context.Context, requires []string, exclude []string) []Candidate {
	req, err := ability.Compile(requires)
	if err != nil {
		return nil
	}
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
		if !c.Match(req).OK() {
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
	req, err := ability.Compile(requires)
	if err != nil {
		return err.Error()
	}
	var reasons []string
	for _, c := range r.All(ctx) {
		switch {
		case !c.Eligible:
			reasons = append(reasons, c.Agent.ID+": "+c.Why)
		default:
			if m := c.Match(req); !m.OK() {
				reasons = append(reasons, c.Agent.ID+": lacks "+m.Unmet())
			}
		}
	}
	if len(reasons) == 0 {
		return "no agents are configured"
	}
	return strings.Join(reasons, "; ")
}

func describe(a agent.Agent, byNode map[string]node.Status, hubCaps []string, hub place, levels map[string]project.Level) Candidate {
	c := Candidate{Agent: a, Node: a.Node, Harness: a.Harness, Model: a.Model, Eligible: true, Level: levels[a.Node].OrDefault()}
	if a.Model != "" {
		c.Models = []string{a.Model}
	}
	if a.Node == "" {
		c.Up = true
		c.Capabilities = hubCaps
		adv := hub.advert
		if len(adv.Capabilities) == 0 {
			adv.Capabilities = hubCaps
		}
		c.Snapshot = nodewire.Synthesize(adv, time.Now())
		c.Level = hub.level.OrDefault()
		c.Slots = hub.slots[a.Harness]
		// A hub that has checked itself is held to the same standard as a
		// node; one that has not (tests, an older wiring) is trusted.
		if len(hub.advert.Harnesses) > 0 {
			c.Command, c.Missing = harnessCommand(hub.advert, a.Harness)
			if missing := harnessTrouble(hub.advert, a.Harness); missing != "" {
				c.Eligible, c.Why = false, missing
				return c
			}
			if offered := harnessModels(hub.advert, a.Harness); len(offered) > 0 {
				c.Models = offered
				if a.Model != "" && !contains(offered, a.Model) {
					c.Eligible, c.Why = false, "this machine does not offer model "+a.Model
					return c
				}
			}
		}
	} else {
		status, known := byNode[a.Node]
		if !known {
			c.Eligible, c.Why = false, "node "+a.Node+" is not configured"
			return c
		}
		c.Up = status.Up
		c.Capabilities = status.Advert.Capabilities
		c.Snapshot = nodewire.Synthesize(status.Advert, time.Now())
		c.Command, c.Missing = harnessCommand(status.Advert, a.Harness)
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
		// Capacity is not capability: a machine with no room for a
		// worktree is not a machine to place work on today.
		if h := status.Advert.Health; h != nil && h.DiskTotal > 0 && h.DiskFree < MinDiskFree {
			c.Eligible, c.Why = false, fmt.Sprintf("disk nearly full on %s: %s free", nodewire.Place(a.Node), gigabytes(h.DiskFree))
			return c
		}
		c.Slots = harnessSlots(status.Advert, a.Harness)
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
	c.addModels(c.Models, "", time.Time{})
	if req, err := ability.Compile(a.Requires); err != nil {
		c.Eligible, c.Why = false, err.Error()
	} else if m := c.Match(req); !m.OK() {
		c.Eligible = false
		c.Why = "lacks " + m.Unmet()
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

func harnessCommand(advert nodewire.Advert, harnessID string) (command, missing string) {
	for _, h := range advert.Harnesses {
		if h.ID == harnessID {
			return h.Command, h.Missing
		}
	}
	return "", ""
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

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// Match evaluates a requirement against this candidate's machine for its
// harness, now. Nil snapshots yield unknown, never a match.
func (c Candidate) Match(req ability.Requirement) ability.MatchResult {
	return ability.Match(req, c.Snapshot, c.Harness, time.Now())
}

// addModels records models this harness was seen running as observed,
// harness-scoped capabilities on a copy of the machine's snapshot: the
// machine's report stays the machine's.
func (c *Candidate) addModels(models []string, version string, at time.Time) {
	if len(models) == 0 || c.Snapshot == nil {
		return
	}
	copied := *c.Snapshot
	copied.Offers = append([]ability.Capability(nil), c.Snapshot.Offers...)
	// The coverage map is copied too: the snapshot is shared by every
	// reader of the roster, and a write to a shared map from two requests
	// at once is fatal, not merely racy.
	copied.Coverage = make(map[ability.Kind]ability.Coverage, len(c.Snapshot.Coverage)+1)
	for k, v := range c.Snapshot.Coverage {
		copied.Coverage[k] = v
	}
	seen := map[string]bool{}
	for _, o := range copied.Offers {
		seen[o.Key()] = true
	}
	for _, m := range models {
		cap := ability.Capability{Kind: ability.Model, ID: m, Scope: c.Harness, Assurance: ability.Functional, Detail: version,
			Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "session", OK: true, At: at}}}
		if seen[cap.Key()] {
			continue
		}
		copied.Offers = append(copied.Offers, cap)
	}
	copied.Coverage[ability.Model] = ability.Complete
	_ = ability.Validate(&copied)
	c.Snapshot = &copied
}

// MinDiskFree is the room a machine must have left to take new work.
const MinDiskFree = 1 << 30

func gigabytes(b uint64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

// markFunctional raises the harness's assurance on a copy of the snapshot:
// a session or probe reached it and it answered.
func (c *Candidate) markFunctional(version string, at time.Time) {
	if c.Snapshot == nil {
		return
	}
	copied := *c.Snapshot
	copied.Offers = append([]ability.Capability(nil), c.Snapshot.Offers...)
	copied.Coverage = make(map[ability.Kind]ability.Coverage, len(c.Snapshot.Coverage))
	for k, v := range c.Snapshot.Coverage {
		copied.Coverage[k] = v
	}
	for i, o := range copied.Offers {
		if o.Kind != ability.Harness || o.ID != c.Harness || o.Availability != ability.Available {
			continue
		}
		o.Evidence = append(append([]ability.Evidence(nil), o.Evidence...), ability.Evidence{Kind: ability.Observed, Method: "acp", OK: true, Result: version, At: at})
		o.Assurance = ability.Functional
		copied.Offers[i] = o
	}
	_ = ability.Validate(&copied)
	c.Snapshot = &copied
}

// Fix is a repair the fleet can attempt: a broken agent, a healthy agent
// on the same machine to do the work, and the command whose presence
// proves it done.
type Fix struct {
	Broken  Candidate
	Helper  Candidate
	Command string
}

// Repair says how a blocked agent could be fixed, or why it cannot be. The
// only repair the fleet knows is "the harness's binary is missing on that
// machine": that is something another agent there can install. A node that
// is down, or a model the harness does not offer, is not fixed by running
// something on the node.
func (r *Roster) Repair(ctx context.Context, agentID string) (Fix, error) {
	all := r.All(ctx)
	var broken *Candidate
	for i := range all {
		if all[i].Agent.ID == agentID {
			broken = &all[i]
		}
	}
	if broken == nil {
		return Fix{}, fmt.Errorf("no agent %q", agentID)
	}
	if broken.Eligible {
		return Fix{}, ErrNotBroken
	}
	if !broken.Up {
		return Fix{}, fmt.Errorf("%s: nothing on that machine can run", broken.Why)
	}
	if broken.Missing == "" || broken.Command == "" {
		return Fix{}, fmt.Errorf("%s: not a missing binary, so not something to install", broken.Why)
	}
	var helper *Candidate
	for i := range all {
		c := &all[i]
		if c.Agent.ID == agentID || c.Node != broken.Node || !c.Eligible {
			continue
		}
		if helper == nil || helperRank(c.Harness) < helperRank(helper.Harness) ||
			(helperRank(c.Harness) == helperRank(helper.Harness) && c.Agent.ID < helper.Agent.ID) {
			helper = c
		}
	}
	if helper == nil {
		return Fix{}, fmt.Errorf("no healthy agent on %s to do the repair", nodewire.Place(broken.Node))
	}
	return Fix{Broken: *broken, Helper: *helper, Command: broken.Command}, nil
}

// ErrNotBroken is Repair's answer for an agent that is fine.
var ErrNotBroken = errors.New("agent is not broken")

// helperRank prefers harnesses that are good at installing things on a
// machine; anything else is a fallback.
func helperRank(harness string) int {
	switch harness {
	case "claude-code":
		return 0
	case "codex":
		return 1
	default:
		return 2
	}
}

// place is how the hub describes its own machine to describe().
type place struct {
	level  project.Level
	slots  map[string]int
	advert nodewire.Advert
}

func harnessSlots(advert nodewire.Advert, id string) int {
	for _, h := range advert.Harnesses {
		if h.ID == id {
			return h.Slots
		}
	}
	return 0
}
