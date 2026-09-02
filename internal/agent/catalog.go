package agent

import (
	"fmt"
	"sort"
	"strings"
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

type Catalog struct {
	agents         map[string]Agent
	aliases        map[string]string
	longestAliases []string
	defaultAgent   Agent
}

func NewCatalog(configs map[string]Config) (*Catalog, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("at least one agent is required")
	}
	c := &Catalog{agents: make(map[string]Agent, len(configs)), aliases: map[string]string{}}
	for configuredID, cfg := range configs {
		id := normalize(configuredID)
		if id == "" || cfg.Harness == "" {
			return nil, fmt.Errorf("agent id and harness are required")
		}
		if _, exists := c.agents[id]; exists {
			return nil, fmt.Errorf("duplicate agent id %q", id)
		}
		agent := Agent{ID: id, Config: cfg}
		c.agents[id] = agent
		for _, alias := range append([]string{id}, cfg.Aliases...) {
			alias = normalize(alias)
			if alias == "" {
				return nil, fmt.Errorf("agent %q has an empty alias", id)
			}
			if owner, exists := c.aliases[alias]; exists && owner != id {
				return nil, fmt.Errorf("agent alias %q is used by %q and %q", alias, owner, id)
			}
			c.aliases[alias] = id
		}
		if cfg.Default {
			if c.defaultAgent.ID != "" {
				return nil, fmt.Errorf("multiple default agents")
			}
			c.defaultAgent = agent
		}
	}
	if c.defaultAgent.ID == "" {
		return nil, fmt.Errorf("one default agent is required")
	}
	c.longestAliases = make([]string, 0, len(c.aliases))
	for alias := range c.aliases {
		c.longestAliases = append(c.longestAliases, alias)
	}
	sort.Slice(c.longestAliases, func(i, j int) bool {
		return len(c.longestAliases[i]) > len(c.longestAliases[j])
	})
	return c, nil
}

func (c *Catalog) Resolve(name string) (Agent, bool) {
	id, ok := c.aliases[normalize(name)]
	if !ok {
		return Agent{}, false
	}
	return c.agents[id], true
}

func (c *Catalog) Default() Agent { return c.defaultAgent }

func (c *Catalog) List() []Agent {
	agents := make([]Agent, 0, len(c.agents))
	for _, item := range c.agents {
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
	for _, alias := range c.longestAliases {
		if strings.HasPrefix(normalized, alias) {
			agent := c.agents[c.aliases[alias]]
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
