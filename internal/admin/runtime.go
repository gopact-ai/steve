package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/skills"
	steveview "github.com/gopact-ai/steve/internal/view"
)

type LocalObservation struct {
	Launch *node.LaunchProbe
	Skills atomic.Pointer[SkillShipper]
}

// HomeProjectID names Steve's home directory as a project.
const HomeProjectID = config.ReservedHomeProject

// HarnessSummary renders a node's runtimes for one log line, marking the
// ones it cannot actually start.
func HarnessSummary(advert nodewire.Advert) string {
	if len(advert.Harnesses) == 0 {
		return "none"
	}
	names := make([]string, 0, len(advert.Harnesses))
	for _, h := range advert.Harnesses {
		if h.Missing != "" {
			names = append(names, withSlots(h)+"(missing)")
			continue
		}
		names = append(names, withSlots(h))
	}
	return strings.Join(names, ",")
}

func withSlots(h nodewire.Harness) string {
	if h.Slots > 0 {
		return fmt.Sprintf("%s(%d)", h.ID, h.Slots)
	}
	return h.ID
}

// hubGeneration identifies this hub process for its own snapshots;
// hubSequence counts them.
var (
	hubGeneration = time.Now().Unix()
	hubSequence   atomic.Int64
)

func ObservedHubAdvert(cfg *config.Config, observation *LocalObservation) nodewire.Advert {
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	specs := make(map[string]node.HarnessSpec, len(cfg.Harnesses))
	for id, h := range cfg.Harnesses {
		specs[id] = node.HarnessSpec{Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir, Slots: h.Slots}
	}
	adv := node.Advertise(NodeName(), specs, cfg.Gateway.Capabilities)
	mcp := make(map[string]node.MCPSpec, len(cfg.MCPServers))
	for id, m := range cfg.MCPServers {
		mcp[id] = node.MCPSpec{Type: m.Type, Command: m.Command, Args: m.Args, URL: m.URL}
	}
	var shipper *SkillShipper
	var launch func(string) (node.LaunchResult, bool)
	if observation != nil {
		shipper = observation.Skills.Load()
		if observation.Launch != nil {
			launch = observation.Launch.Lookup
		}
	}
	entries, known := shipper.entries()
	adv.Snapshot = node.Snapshot(NodeName(), hubGeneration, hubSequence.Add(1), node.Observe{Harnesses: specs, Tools: cfg.Gateway.Tools, MCP: mcp, Declares: cfg.Gateway.Declares, Tags: cfg.Gateway.Capabilities, Launch: launch, Skills: entries, SkillsKnown: known})
	adv.Features = nodewire.Features()
	adv.OwnSkills = node.OwnSkills(5 * time.Minute)
	adv.StateDir = filepath.Dir(cfg.Gateway.StatePath)
	adv.WorkspaceRoot = cfg.LocalWorkspaceRoot()
	// The hub is a machine like any other: it measures the workspace it
	// was given, not only the directory it keeps its state in.
	adv.Health = node.CheckHealth(adv.WorkspaceRoot, adv.StateDir)
	return adv
}

// repoCache keeps what each project's directory holds, asked of its
// machine every minute and whenever a project is added.
type RepoCache struct {
	Projects *project.Store
	Nodes    *node.Registry
	Hub      string

	mu    sync.Mutex
	repos map[string][]nodewire.Repo
	Poke  chan struct{}
}

func (c *RepoCache) Get(id string) []nodewire.Repo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repos[id]
}

func (c *RepoCache) wake() {
	select {
	case c.Poke <- struct{}{}:
	default:
	}
}

func (c *RepoCache) Run(ctx context.Context) {
	for {
		c.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		case <-c.Poke:
		}
	}
}

func (c *RepoCache) pass(ctx context.Context) {
	list, err := c.Projects.List(ctx)
	if err != nil {
		return
	}
	next := make(map[string][]nodewire.Repo, len(list))
	for _, p := range list {
		for _, ws := range p.Workspaces() {
			var repos []nodewire.Repo
			if ws.Node == "" || ws.Node == c.Hub {
				repos = node.InspectRepos(ctx, ws.Path)
			} else {
				ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
				repos, err = c.Nodes.Inspect(ictx, ws.Node, ws.Path)
				cancel()
				if err != nil {
					// Keep what we knew: a machine that is down did not
					// lose its repositories.
					repos = c.Get(ws.ID)
				}
			}
			next[ws.ID] = repos
		}
	}
	c.mu.Lock()
	c.repos = next
	c.mu.Unlock()
}

// selectorsOf keeps every selector a session exposed, choices by label
// and by value, so the page can offer them and a pin can be matched.
func SelectorsOf(options []steveview.Option) []models.Selector {
	var out []models.Selector
	for _, o := range options {
		sel := models.Selector{ID: o.ID, Name: o.Name, Category: o.Category, Current: o.Current}
		for _, c := range o.Choices {
			label := c.Label
			if label == "" {
				label = c.Value
			}
			sel.Choices = append(sel.Choices, label)
			sel.Values = append(sel.Values, c.Value)
		}
		out = append(out, sel)
	}
	return out
}

// skillShipper keeps every node's harness homes holding the same skills
// the hub enabled. The bundle is packed from the live map each time it is
// needed — skills are small, and a stale bundle would ship stale skills.
type SkillShipper struct {
	Nodes   *node.Registry
	Live    *skills.Live
	Observe func(kind, subject, text string, data map[string]string)

	mu     sync.Mutex
	bundle skills.Bundle
	packed bool
}

// ErrSkillsPending means desired skills remain saved but a configured node
// is offline. It is not an acknowledgement that the fleet applied the bundle.
var ErrSkillsPending = errors.New("skill propagation pending")

func (s *SkillShipper) Pack() (skills.Bundle, error) {
	if s == nil || s.Live == nil || s.Live.Map == nil {
		return skills.Bundle{}, errors.New("skills are not configured")
	}
	refs, err := s.Live.Map.Enabled()
	if err != nil {
		return skills.Bundle{}, err
	}
	refs, err = skills.ResolveRefs(refs)
	if err != nil {
		return skills.Bundle{}, err
	}
	b, err := skills.Pack(refs)
	if err != nil {
		return skills.Bundle{}, err
	}
	s.mu.Lock()
	s.bundle, s.packed = b, true
	s.mu.Unlock()
	return b, nil
}

// entries is what the hub's own machine has: the last packed bundle.
// hash is the bundle the hub last packed, or "" before the first pack.
func (s *SkillShipper) hash() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.packed {
		return ""
	}
	return s.bundle.Hash
}

func (s *SkillShipper) entries() ([]skills.Entry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bundle.Skills, s.packed
}

// Ship returns propagation errors and also logs them for asynchronous node-up
// callers. It never restarts harnesses; the caller owns that decision.
func (s *SkillShipper) Ship(ctx context.Context, name string) error {
	b, err := s.Pack()
	if err != nil {
		slog.Error(fmt.Sprintf("steve: skills for %s: %v", name, err), "node", name)
		return fmt.Errorf("pack skills for node %q: %w", name, err)
	}
	if s.Nodes != nil {
		for _, st := range s.Nodes.Statuses() {
			if st.Name == name {
				return s.ship(ctx, st, b)
			}
		}
	}
	err = fmt.Errorf("skill destination node %q is not configured", name)
	slog.Error("steve: skills could not be shipped", "node", name, "error", err)
	return err
}

func (s *SkillShipper) ship(ctx context.Context, st node.Status, b skills.Bundle) error {
	name := st.Name
	err := ctx.Err()
	if err == nil {
		if !st.Up {
			// Do not wait for connectivity while saving desired state.
			err = fmt.Errorf("%w: node %q is offline; desired skills remain saved", ErrSkillsPending, name)
		} else {
			sctx, cancel := context.WithTimeout(ctx, time.Minute)
			err = s.Nodes.PushSkills(sctx, name, b)
			cancel()
		}
	}
	if err != nil {
		slog.Error(fmt.Sprintf("steve: skills to %s: %v", name, err), "node", name)
		if s.Observe != nil {
			// Keys: hash, error.
			s.Observe("node.skills", name, fmt.Sprintf("%s: skills %s not materialized: %v", name, b.Hash[:12], err), map[string]string{"hash": b.Hash[:12], "error": err.Error()})
		}
		return fmt.Errorf("ship skills to node %q: %w", name, err)
	}
	if s.Observe != nil {
		// Keys: hash, count.
		s.Observe("node.skills", name, fmt.Sprintf("%s: skills %s materialized (%d skills)", name, b.Hash[:12], len(b.Skills)), map[string]string{"hash": b.Hash[:12], "count": strconv.Itoa(len(b.Skills))})
	}
	return nil
}

// ShipAll sends one desired bundle to every online node, collecting failures
// without skipping other nodes. Offline nodes remain pending, not applied.
func (s *SkillShipper) ShipAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := s.Pack()
	if err != nil {
		slog.Error("steve: skills could not be packed for nodes", "error", err)
		return fmt.Errorf("pack skills for nodes: %w", err)
	}
	if s.Nodes == nil {
		return nil
	}
	var failures []error
	for _, st := range s.Nodes.Statuses() {
		if err := s.ship(ctx, st, b); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// nodeName labels which machine ran a turn: the hub's own node name.
var LocalNodeIdentity atomic.Value

func NodeName() string {
	if id, ok := LocalNodeIdentity.Load().(string); ok && id != "" {
		return id
	}
	if name := strings.TrimSpace(os.Getenv("STEVE_NODE")); name != "" {
		return name
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "local"
	}
	return host
}
