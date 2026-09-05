package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/protocol"
)

type Config struct {
	Harness string
	// Node is the machine this agent runs on; empty is the hub itself.
	// An agent is (node, harness, model): the harness binary and the model
	// endpoints it can reach are properties of a machine, so the same
	// harness id on two hosts is two different agents.
	Node string
	// Model pins which model the agent starts on. The node's advert is the
	// authority on what is actually offered there; this is the preference.
	Model string
	// Options pins other selectors the harness exposes, by option id:
	// reasoning effort, thinking, mode. Applied at session open like Model.
	Options map[string]string
	// About says what this agent is for, in the operator's words; the
	// planner and other agents read it when choosing who does what.
	About string
	// Requires are the capabilities a node must advertise to run this
	// agent — "gpu", "prod-cred", "internal-net".
	Requires     []string
	Aliases      []string
	SystemPrompt string
	Skills       []string
	MCPServers   []string
	Default      bool
}

type Agent struct {
	ID string
	Config
}

// Catalog is the agents by id and alias. Its contents are built once and
// replaced whole: readers take the current set without locking, and an
// Add builds a new set from every config so the same rules apply to an
// agent added from the page as to one from the file.
type Catalog struct {
	d       atomic.Pointer[catalogData]
	mu      sync.Mutex
	configs map[string]Config
}

type catalogData struct {
	agents         map[string]Agent
	aliases        map[string]string
	longestAliases []string
	defaultAgent   Agent
}

func (c *Catalog) snap() *catalogData { return c.d.Load() }

// Set replaces one agent's configuration, or adds it. What is not in cfg
// is gone: the caller passes the whole thing.
func (c *Catalog) Set(id string, cfg Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	all := make(map[string]Config, len(c.configs)+1)
	for k, v := range c.configs {
		all[k] = v
	}
	all[id] = cfg
	fresh, err := NewCatalog(all)
	if err != nil {
		return err
	}
	c.configs = all
	c.d.Store(fresh.snap())
	return nil
}

// Remove forgets an agent. The default agent stays: a catalog needs one.
func (c *Catalog) Remove(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg, ok := c.configs[id]
	if !ok {
		return fmt.Errorf("no agent %q", id)
	}
	if cfg.Default {
		return fmt.Errorf("%s is the default agent; make another the default first", id)
	}
	all := make(map[string]Config, len(c.configs))
	for k, v := range c.configs {
		if k != id {
			all[k] = v
		}
	}
	fresh, err := NewCatalog(all)
	if err != nil {
		return err
	}
	c.configs = all
	c.d.Store(fresh.snap())
	return nil
}

// Add puts one more agent in the catalog, or fails with why it cannot:
// a duplicate id or alias, a missing harness. The file the hub was loaded
// from is the caller's to update.
func (c *Catalog) Add(id string, cfg Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	all := make(map[string]Config, len(c.configs)+1)
	for k, v := range c.configs {
		all[k] = v
	}
	if _, exists := all[id]; exists {
		return fmt.Errorf("agent %q already exists", id)
	}
	all[id] = cfg
	fresh, err := NewCatalog(all)
	if err != nil {
		return err
	}
	c.configs = all
	c.d.Store(fresh.snap())
	return nil
}

func NewCatalog(configs map[string]Config) (*Catalog, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("at least one agent is required")
	}
	d := &catalogData{agents: make(map[string]Agent, len(configs)), aliases: map[string]string{}}
	for configuredID, cfg := range configs {
		id := normalize(configuredID)
		if id == "" || cfg.Harness == "" {
			return nil, fmt.Errorf("agent id and harness are required")
		}
		if _, exists := d.agents[id]; exists {
			return nil, fmt.Errorf("duplicate agent id %q", id)
		}
		agent := Agent{ID: id, Config: cfg}
		d.agents[id] = agent
		for _, alias := range append([]string{id}, cfg.Aliases...) {
			alias = normalize(alias)
			if alias == "" {
				return nil, fmt.Errorf("agent %q has an empty alias", id)
			}
			if owner, exists := d.aliases[alias]; exists && owner != id {
				return nil, fmt.Errorf("agent alias %q is used by %q and %q", alias, owner, id)
			}
			d.aliases[alias] = id
		}
		if cfg.Default {
			if d.defaultAgent.ID != "" {
				return nil, fmt.Errorf("multiple default agents")
			}
			d.defaultAgent = agent
		}
	}
	if d.defaultAgent.ID == "" {
		return nil, fmt.Errorf("one default agent is required")
	}
	d.longestAliases = make([]string, 0, len(d.aliases))
	for alias := range d.aliases {
		d.longestAliases = append(d.longestAliases, alias)
	}
	sort.Slice(d.longestAliases, func(i, j int) bool {
		return len(d.longestAliases[i]) > len(d.longestAliases[j])
	})
	c := &Catalog{configs: make(map[string]Config, len(configs))}
	for k, v := range configs {
		c.configs[k] = v
	}
	c.d.Store(d)
	return c, nil
}

func (c *Catalog) Resolve(name string) (Agent, bool) {
	id, ok := c.snap().aliases[normalize(name)]
	if !ok {
		return Agent{}, false
	}
	return c.snap().agents[id], true
}

func (c *Catalog) Default() Agent { return c.snap().defaultAgent }

func (c *Catalog) List() []Agent {
	agents := make([]Agent, 0, len(c.snap().agents))
	for _, item := range c.snap().agents {
		agents = append(agents, item)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	return agents
}

type Selection struct {
	Agent      Agent
	Prompt     string
	SwitchOnly bool
}

func (c *Catalog) Select(input string) (Selection, bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return Selection{}, false
	}
	var rest string
	switch {
	case strings.HasPrefix(input, "@"):
		rest = strings.TrimPrefix(input, "@")
	case strings.HasPrefix(input, string(protocol.CommandUse)) && hasLeadingSpace(strings.TrimPrefix(input, string(protocol.CommandUse))):
		rest = strings.TrimSpace(strings.TrimPrefix(input, string(protocol.CommandUse)))
	default:
		return Selection{}, false
	}
	name, prompt := cutField(rest)
	agent, ok := c.Resolve(name)
	if !ok {
		// Chinese input often omits the space after the tag ("@codex帮我看看")
		// or uses fullwidth punctuation; fall back to longest-alias-prefix
		// matching so the message still reaches the intended agent.
		agent, prompt, ok = c.resolvePrefix(rest)
		if !ok {
			return Selection{}, false
		}
	}
	return Selection{Agent: agent, Prompt: prompt, SwitchOnly: prompt == ""}, true
}

// resolvePrefix matches the longest configured alias that is a prefix of
// rest and returns the remainder as the prompt.
func (c *Catalog) resolvePrefix(rest string) (Agent, string, bool) {
	rest = strings.TrimSpace(rest)
	normalized := normalize(rest)
	for _, alias := range c.snap().longestAliases {
		if strings.HasPrefix(normalized, alias) {
			agent := c.snap().agents[c.snap().aliases[alias]]
			return agent, strings.TrimSpace(rest[len(alias):]), true
		}
	}
	return Agent{}, "", false
}

func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func cutField(value string) (string, string) {
	index := strings.IndexFunc(value, unicode.IsSpace)
	if index < 0 {
		return value, ""
	}
	return value[:index], strings.TrimSpace(value[index:])
}

func hasLeadingSpace(value string) bool {
	first, _ := utf8.DecodeRuneInString(value)
	return unicode.IsSpace(first)
}
