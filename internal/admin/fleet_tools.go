package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// fleetTools answers the platform MCP server's fleet questions and
// changes from the admin and the read model.
type FleetTools struct {
	Admin *Service
	View  *readmodel.Model
}

func (f FleetTools) Nodes(ctx context.Context) (string, error) {
	snap := f.View.Snapshot(ctx)
	type harness struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Model string `json:"model,omitempty"`
		Why   string `json:"why,omitempty"`
	}
	type machine struct {
		Name      string    `json:"name"`
		Role      string    `json:"role"`
		Up        bool      `json:"up"`
		Since     string    `json:"since,omitempty"`
		LastError string    `json:"last_error,omitempty"`
		Version   string    `json:"version,omitempty"`
		Level     string    `json:"level,omitempty"`
		OS        string    `json:"os,omitempty"`
		DiskFree  string    `json:"disk_free,omitempty"`
		Load      float64   `json:"load1,omitempty"`
		Worktrees int       `json:"worktrees,omitempty"`
		Harnesses []harness `json:"harnesses"`
		MCP       []string  `json:"mcp_servers"`
		OwnSkills int       `json:"own_skills"`
		Agents    []string  `json:"agents"`
	}
	agentsOn := map[string][]string{}
	for _, ag := range snap.Agents {
		agentsOn[ag.Node] = append(agentsOn[ag.Node], ag.ID)
	}
	out := make([]machine, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		m := machine{Name: n.Name, Role: n.Role, Up: n.Up, Version: n.Version, Level: n.Level, OS: n.OS, LastError: n.LastError, Harnesses: []harness{}, MCP: []string{}, Agents: agentsOn[n.Name]}
		if !n.Since.IsZero() {
			m.Since = n.Since.UTC().Format(time.RFC3339)
		}
		if n.Health != nil && n.Health.DiskTotal > 0 {
			m.DiskFree = fmt.Sprintf("%.0f GB", float64(n.Health.DiskFree)/(1<<30))
			m.Load, m.Worktrees = n.Health.Load1, n.Health.Worktrees
		}
		for _, h := range n.Harnesses {
			state := "ready"
			if h.Missing != "" {
				state = "missing"
			}
			m.Harnesses = append(m.Harnesses, harness{ID: h.ID, State: state, Model: h.Model, Why: h.Missing})
		}
		if n.Snapshot != nil {
			for _, c := range n.Snapshot.Offers {
				if c.Kind == ability.MCP {
					m.MCP = append(m.MCP, c.ID)
				}
			}
		}
		if adv, err := f.Admin.advertOf(ctx, f.Admin.nodeKey(n.Name)); err == nil {
			m.OwnSkills = len(adv.OwnSkills)
		}
		if m.Agents == nil {
			m.Agents = []string{}
		}
		sort.Strings(m.MCP)
		out = append(out, m)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	return string(raw), err
}

func (f FleetTools) AddNode(ctx context.Context, name, addr, level, hubURL string) (string, error) {
	if hubURL == "" {
		f.Admin.Mu.Lock()
		hubURL = f.Admin.hubURL
		f.Admin.Mu.Unlock()
	}
	if hubURL == "" {
		return "", errors.New("需要 hub_url：那台机器怎么访问 hub 的控制台，如 http://10.0.0.1:7710")
	}
	res, err := f.Admin.AddNode(ctx, consoleapi.AddNodeRequest{Name: name, Addr: addr, Level: level, HubURL: hubURL})
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("机器 %s 已登记（%s）。在那台机器上以登录 shell 跑这一条，它会装好 steve-node 并连上来：\n\n%s", res.Name, addr, res.Command)
	if res.Note != "" {
		text += "\n\n" + res.Note
	}
	return text, nil
}

func (f FleetTools) RemoveNode(ctx context.Context, name string) error {
	return f.Admin.RemoveNode(ctx, name)
}

func (f FleetTools) RefreshNode(ctx context.Context, name string) (string, error) {
	key := f.Admin.nodeKey(name)
	if key == "" {
		return "", errors.New("hub 自己不用刷新，它的申报是现算的")
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	adv, err := f.Admin.Nodes.Refresh(rctx, key)
	if err != nil {
		return "", err
	}
	summary := map[string]any{"name": name, "version": adv.BuildVersion, "harnesses": len(adv.Harnesses), "own_skills": len(adv.OwnSkills), "own_mcp": len(adv.OwnMCP), "features": adv.Features}
	if adv.Health != nil && adv.Health.DiskTotal > 0 {
		summary["disk_free_gb"] = adv.Health.DiskFree >> 30
		summary["load1"] = adv.Health.Load1
	}
	raw, err := json.MarshalIndent(summary, "", "  ")
	return string(raw), err
}
