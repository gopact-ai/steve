package agent

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Config struct {
	Harness      string
	Aliases      []string
	Workspace    string
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
	agents       map[string]Agent
	aliases      map[string]string
	defaultAgent Agent
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
	var name string
	var prompt string
	switch {
	case strings.HasPrefix(input, "@"):
		name, prompt = cutField(strings.TrimPrefix(input, "@"))
	case strings.HasPrefix(input, "/use") && hasLeadingSpace(strings.TrimPrefix(input, "/use")):
		remainder := strings.TrimSpace(strings.TrimPrefix(input, "/use"))
		name, prompt = cutField(remainder)
	default:
		return Selection{}, false
	}
	agent, ok := c.Resolve(name)
	if !ok {
		return Selection{}, false
	}
	return Selection{Agent: agent, Prompt: prompt, SwitchOnly: prompt == ""}, true
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
