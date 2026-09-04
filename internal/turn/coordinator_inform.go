package turn

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

// Whereabouts is where a session stands, for the platform's own MCP
// tool: the facts an agent used to be told once, in the first prompt,
// answered live whenever it asks.
type Whereabouts struct {
	Agent, Node, Harness, Model string
	Mode                        string
	Project, ProjectNode        string
	Level, Repo                 string
	Workspace, WorkspaceKind    string
	Why                         string
	Task                        string
	Turns, MaxTurns             int
	Elapsed, MaxElapsed         string
	MCPServers, Skills          []string
}

// rememberMode keeps how a conversation last reached Steve: the owner
// in private, or anyone else. A tool call later has no request to read
// it from.
func (c *Coordinator) rememberMode(req Request) {
	mode := injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID)
	c.mu.Lock()
	if c.modes == nil {
		c.modes = map[string]home.Mode{}
	}
	c.modes[req.ConversationID] = mode
	c.mu.Unlock()
}

func (c *Coordinator) modeOf(conversationID string) home.Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.modes[conversationID]; ok {
		return m
	}
	return home.ModeGuest
}

// Where answers steve_context.
func (c *Coordinator) Where(ctx context.Context, conversationID, agentID string) (Whereabouts, error) {
	if c.catalog == nil {
		return Whereabouts{}, errors.New("no agent catalog")
	}
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return Whereabouts{}, fmt.Errorf("no agent %q", agentID)
	}
	w := Whereabouts{Agent: selected.ID, Node: placeLabel(selected.Node), Harness: selected.Harness, Model: selected.Model, Mode: string(c.modeOf(conversationID)), MCPServers: append([]string{"steve"}, selected.MCPServers...), Skills: []string{}}
	if c.skills != nil && c.skills.Map != nil {
		w.Skills = c.skills.Map.EnabledNames()
	}
	if c.projects != nil {
		if id, _, _, err := c.projectFor(ctx, conversationID); err == nil && id != "" {
			if p, found, err := c.projects.Get(ctx, id); err == nil && found {
				w.Project, w.ProjectNode, w.Level, w.Repo = p.ID, placeLabel(p.Home.Node), string(p.Level.OrDefault()), string(p.Repo)
				if ws, err := p.Place(selected.Node); err == nil {
					w.Workspace, w.WorkspaceKind = ws.Path, string(ws.Kind)
				} else {
					var notHome project.NotHomeError
					places := placeLabel(p.Home.Node)
					if errors.As(err, &notHome) {
						places = notHome.PlaceList()
					}
					w.Why = fmt.Sprintf("项目 %s 的工作区在 %s，你在 %s：换一个那里的 Agent（/use），请用户在 %s 上给项目添加副本，或 %s use 换项目", p.ID, places, w.Node, w.Node, protocol.CommandProject)
				}
			}
		}
	}
	if c.tasks != nil {
		if t, running := c.tasks.Running(conversationID, agentID); running {
			w.Task = t.ID
			w.Turns, w.MaxTurns = t.Budget.Turns, t.Budget.MaxTurns
			w.Elapsed, w.MaxElapsed = t.Budget.Elapsed.Round(time.Second).String(), t.Budget.MaxElapsed.Round(time.Second).String()
		}
	}
	sort.Strings(w.MCPServers)
	return w, nil
}

// WhereProjects answers steve_projects: every project, where it is, and
// whether this agent can work in it from its machine.
func (c *Coordinator) WhereProjects(ctx context.Context, conversationID, agentID string) (string, error) {
	if c.projects == nil {
		return "", errors.New("projects are not enabled")
	}
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return "", fmt.Errorf("no agent %q", agentID)
	}
	current, _, _, _ := c.projectFor(ctx, conversationID)
	all, err := c.projects.List(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "你是 %s，在 %s。当前项目：%s\n\n", selected.ID, placeLabel(selected.Node), orNone(current))
	for _, p := range all {
		marker := "  "
		if p.ID == current {
			marker = "* "
		}
		var places []string
		for _, ws := range p.Workspaces() {
			kind := "主目录"
			if ws.Kind == project.KindCopy {
				kind = "副本"
			}
			places = append(places, fmt.Sprintf("%s@%s:%s", kind, placeLabel(ws.Node), ws.Path))
		}
		can := "你在这台机器上不能接它"
		if ws, err := p.Place(selected.Node); err == nil {
			can = "你可以在 " + ws.Path + " 里干活"
		}
		fmt.Fprintf(&b, "%s%s — %s；%s；数据等级 %s，%s\n", marker, p.ID, strings.Join(places, "，"), can, p.Level.OrDefault(), repoWord(p.Repo))
	}
	b.WriteString("\n项目不在你这台机器上时：换一个那台机器上的 Agent（/use），请用户在你这台机器上给项目添加副本（控制台项目页），或 /project use <id> 换项目。在本机凭空造目录不算。")
	return b.String(), nil
}

func orNone(s string) string {
	if s == "" {
		return "（无）"
	}
	return s
}

func repoWord(mode project.RepoMode) string {
	if mode == project.RepoIsolated {
		return "隔离副本模式（步骤在工作树里改完再合并）"
	}
	return "直接修改主目录"
}

var _ = nodewire.Place
