package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"strings"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

// changeAgents validates a complete candidate before touching durable or live
// state. The persisted candidate is published as one catalog snapshot; there
// is no fallible catalog mutation left after the file has been replaced.
func (a *fleetAdmin) changeAgents(change func(map[string]config.Agent) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	configMu.Lock()
	defer configMu.Unlock()
	candidate := *a.cfg
	candidate.Agents = maps.Clone(a.cfg.Agents)
	if candidate.Agents == nil {
		candidate.Agents = map[string]config.Agent{}
	}
	if err := change(candidate.Agents); err != nil {
		return err
	}
	prepared, err := candidate.AgentCatalog()
	if err != nil {
		return err
	}
	saveErr := a.persistConfig(&candidate)
	if saveErr != nil && !config.Committed(saveErr) {
		return saveErr
	}
	a.catalog.Publish(prepared)
	a.cfg.Agents = candidate.Agents
	return saveErr
}

// UpdateAgent preserves fields that are not editable through this endpoint.
func (a *fleetAdmin) UpdateAgent(_ context.Context, id string, spec consoleapi.AgentSpec) error {
	id = strings.ToLower(strings.TrimSpace(id))
	err := a.changeAgents(func(agents map[string]config.Agent) error {
		item, ok := agents[id]
		if !ok {
			return fmt.Errorf("没有叫 %q 的 Agent", id)
		}
		if _, ok := a.cfg.Harnesses[spec.Harness]; !ok {
			return fmt.Errorf("hub 的 harnesses 里没有 %q", spec.Harness)
		}
		if spec.Node != "" {
			if _, ok := a.cfg.Nodes[spec.Node]; !ok {
				return fmt.Errorf("没有叫 %q 的机器", spec.Node)
			}
		}
		if err := ability.ValidateText(spec.Requires); err != nil {
			return fmt.Errorf("运行条件：%w", err)
		}
		if spec.Node == "" {
			for _, server := range spec.MCPServers {
				if _, ok := a.cfg.MCPServers[server]; !ok {
					return fmt.Errorf("hub 上没有 MCP 服务器 %q；在 hub 机器的配置里加，或把 Agent 放到有它的机器上", server)
				}
			}
		}
		item.Harness, item.Node, item.Model = spec.Harness, spec.Node, strings.TrimSpace(spec.Model)
		item.About = strings.TrimSpace(spec.About)
		item.Options = map[string]string{}
		for key, value := range spec.Options {
			if key = strings.TrimSpace(key); key != "" && strings.TrimSpace(value) != "" {
				item.Options[key] = strings.TrimSpace(value)
			}
		}
		if len(item.Options) == 0 {
			item.Options = nil
		}
		item.Requires = append([]string(nil), spec.Requires...)
		item.MCPServers = append([]string(nil), spec.MCPServers...)
		agents[id] = item
		return nil
	})
	if err == nil || config.Committed(err) {
		log.Printf("steve: agent %s updated (%s on %s)", id, spec.Harness, orHubName(spec.Node))
	}
	return err
}

func (a *fleetAdmin) AddAgent(_ context.Context, req consoleapi.AddAgentRequest) error {
	id := strings.ToLower(strings.TrimSpace(req.ID))
	if !nameShape.MatchString(id) {
		return fmt.Errorf("Agent 名只能是小写字母、数字、点、下划线、连字符")
	}
	err := a.changeAgents(func(agents map[string]config.Agent) error {
		if _, ok := a.cfg.Harnesses[req.Harness]; !ok {
			return fmt.Errorf("hub 的 harnesses 里没有 %q；Agent 用的 AI 工具要先在 hub 配置", req.Harness)
		}
		if req.Node != "" {
			if _, ok := a.cfg.Nodes[req.Node]; !ok {
				return fmt.Errorf("没有叫 %q 的机器", req.Node)
			}
		}
		if _, exists := agents[id]; exists {
			return fmt.Errorf("Agent %s 已经存在", id)
		}
		agents[id] = config.Agent{Harness: req.Harness, Node: req.Node, Model: req.Model}
		return nil
	})
	if err == nil || config.Committed(err) {
		log.Printf("steve: agent %s added (%s on %s)", id, req.Harness, orHubName(req.Node))
	}
	return err
}

func (a *fleetAdmin) RemoveAgent(_ context.Context, id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	err := a.changeAgents(func(agents map[string]config.Agent) error {
		item, ok := agents[id]
		if !ok {
			return fmt.Errorf("没有叫 %q 的 Agent", id)
		}
		if item.Default {
			return fmt.Errorf("%s 是默认 Agent，不能删；先在配置里换一个默认", id)
		}
		delete(agents, id)
		return nil
	})
	if err == nil || config.Committed(err) {
		log.Printf("steve: agent %s removed", id)
	}
	return err
}
