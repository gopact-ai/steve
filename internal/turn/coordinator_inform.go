package turn

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/conversation"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
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
// it from. A line that does not say whether it came in private or in a
// group, such as a replayed card tap, is no evidence and changes nothing.
func (c *Coordinator) rememberMode(req Request) {
	if req.Source.ChatType != protocol.ChatP2P && req.Source.ChatType != protocol.ChatGroup {
		return
	}
	mode := c.requestMode(req)
	c.conversations.RememberMode(req.Source.ConversationID, mode)
}

// consolePrefix is retained for trusted legacy embedding without durable
// routing. Installed channels use admitted route facts, never this prefix.
const consolePrefix = "console:"

// arrivalMode is how a conversation reaches Steve, as far as a read with
// no request in hand can know: a console conversation as the owner in
// private, any other as its last line did. known is false for a channel
// conversation not heard from since this process started — a group, a
// guest and the owner in private look alike by their key.
func (c *Coordinator) arrivalMode(conversationID string) (mode home.Mode, known bool) {
	if c.isConsoleConversation(conversationID) {
		return injectionMode(protocol.ChatP2P, c.owners.ConsoleOwner(), c.owners.ConsoleOwner()), true
	}
	return c.conversations.Mode(conversationID)
}

// isConsoleConversation reads admitted routing facts; opaque native names may
// contain the console prefix without acquiring Console owner authority.
func (c *Coordinator) isConsoleConversation(id string) bool {
	if accounts := c.owners.Configuration().Accounts; accounts != nil {
		route, err := conversation.Channel(context.Background(), accounts.Book, id)
		return err == nil && route == "console"
	}
	// Legacy trusted in-process callers have no installed provider authority.
	return strings.HasPrefix(id, consolePrefix)
}

// modeOf is how a conversation reaches Steve, and guest when that is not
// known: nothing of the owner's is lent on a guess.
func (c *Coordinator) modeOf(conversationID string) home.Mode {
	if mode, known := c.arrivalMode(conversationID); known {
		return mode
	}
	return home.ModeGuest
}

// Where answers steve_context.
func (c *Coordinator) Where(ctx context.Context, conversationID, agentID string) (Whereabouts, error) {
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return Whereabouts{}, fmt.Errorf("no agent %q", agentID)
	}
	w := Whereabouts{Agent: selected.ID, Node: placeLabel(selected.Node), Harness: selected.Harness, Model: selected.Model, Mode: string(c.modeOf(conversationID)), MCPServers: append([]string{agentmcp.ServerName}, selected.MCPServers...), Skills: []string{}}
	projectID, _, _, _ := c.projectFor(ctx, conversationID)
	tracked, running := c.tasks.Running(conversationID, agentID)
	if c.owners.Configuration().Accounts != nil {
		var mode home.Mode
		var err error
		tracked, mode, err = c.platformSubject(ctx, conversationID, agentID)
		if err != nil {
			return Whereabouts{}, err
		}
		projectID, running, w.Mode = tracked.ProjectID, true, string(mode)
	}
	if c.skills.Map != nil {
		w.Skills = c.skills.Map.EnabledNames()
	}
	if id := projectID; id != "" {
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
	if t := tracked; running {
		w.Task = t.ID
		w.Turns, w.MaxTurns = t.Budget.Turns, t.Budget.MaxTurns
		w.Elapsed = t.Budget.Elapsed.Round(time.Second).String()
		if t.Budget.MaxElapsed > 0 {
			w.MaxElapsed = t.Budget.MaxElapsed.Round(time.Second).String()
		}
	}
	sort.Strings(w.MCPServers)
	return w, nil
}

func (c *Coordinator) platformSubject(ctx context.Context, conversationID, agentID string) (tracked task.Task, mode home.Mode, err error) {
	scope, ok := agentmcp.ScopeFromContext(ctx)
	if !ok {
		return tracked, mode, agentmcp.ErrGrantDenied
	}
	accounts := c.owners.Configuration().Accounts
	err = accounts.Book.Read(ctx, func(tx *ledger.ReadTx) error {
		proof, root, err := c.controls.ExecutionCallerTx(tx, scope.AttemptID)
		if err != nil {
			return err
		}
		r, err := attempt.GetTx(tx, scope.AttemptID)
		if err != nil {
			return err
		}
		var found bool
		tracked, found, err = task.GetTx(tx, r.TaskID)
		if err != nil {
			return err
		}
		if !found || tracked.Channel != conversationID || tracked.Member != agentID || tracked.ID != scope.TaskID {
			return agentmcp.ErrGrantDenied
		}
		role, err := project.AccessPolicyTx(tx, tracked.ProjectID, proof.Principal, accounts.Owner)
		if err != nil {
			return err
		}
		if !role.AtLeast(project.RoleRead) {
			return agentmcp.ErrGrantDenied
		}
		mode = home.ModeGuest
		if proof.Principal == accounts.Owner && root.ChatType == string(protocol.ChatP2P) && !tracked.Delegated() {
			mode = home.ModeOwner
		}
		return nil
	})
	return
}

// WhereProjects answers steve_projects: every project, where it is, and
// whether this agent can work in it from its machine.
func (c *Coordinator) WhereProjects(ctx context.Context, conversationID, agentID string) (string, error) {
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return "", fmt.Errorf("no agent %q", agentID)
	}
	current, _, _, _ := c.projectFor(ctx, conversationID)
	if c.owners.Configuration().Accounts != nil {
		t, _, err := c.platformSubject(ctx, conversationID, agentID)
		if err != nil {
			return "", err
		}
		current = t.ProjectID
	}
	all, err := c.projects.List(ctx)
	if err != nil {
		return "", err
	}
	if accounts := c.owners.Configuration().Accounts; accounts != nil {
		scope, ok := agentmcp.ScopeFromContext(ctx)
		if !ok {
			return "", agentmcp.ErrGrantDenied
		}
		err = accounts.Book.Read(ctx, func(tx *ledger.ReadTx) error {
			proof, _, err := c.controls.ExecutionCallerTx(tx, scope.AttemptID)
			if err != nil {
				return err
			}
			visible := all[:0]
			for _, p := range all {
				role, err := project.AccessPolicyTx(tx, p.ID, proof.Principal, accounts.Owner)
				if err != nil {
					return err
				}
				if role.AtLeast(project.RoleRead) {
					visible = append(visible, p)
				}
			}
			all = visible
			return nil
		})
		if err != nil {
			return "", err
		}
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
