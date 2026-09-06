package turn

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/nodewire"
	"log"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
)

// bindingFor returns the conversation's project binding, making the
// default one on first use. The default is recorded like any other
// binding — version 1, by "default" — so a later switch is a visible
// change from something, not from nothing.
func (c *Coordinator) bindingFor(ctx context.Context, req Request) (project.Binding, error) {
	if c.projects == nil {
		return project.Binding{}, UserError{Text: c.text.T(i18n.ProjectsDisabled)}
	}
	binding, ok, err := c.projects.Binding(ctx, req.ConversationID)
	if err != nil {
		return project.Binding{}, err
	}
	if ok {
		return binding, nil
	}
	id := c.defaultProject
	if c.homeProject != "" && injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) == home.ModeOwner {
		id = c.homeProject
	}
	if id == "" {
		return project.Binding{}, UserError{Text: c.text.T(i18n.ProjectUnbound, protocol.CommandProject)}
	}
	binding, err = c.projects.Bind(ctx, req.ConversationID, id, "default")
	if err != nil {
		return project.Binding{}, err
	}
	log.Printf("turn: conversation %s bound to project %s by default", req.ConversationID, id)
	return binding, nil
}

// resolveWorkspace is where an interactive turn learns its directory: the
// conversation's project, materialised on the agent's node. An agent that
// cannot reach the project's canonical workspace is told so in terms of
// places, with the two ways out.
func (c *Coordinator) resolveWorkspace(ctx context.Context, req Request, selected agent.Agent) (project.Binding, project.Workspace, error) {
	binding, err := c.bindingFor(ctx, req)
	if err != nil {
		return project.Binding{}, project.Workspace{}, err
	}
	if err := checkScheduledProject(req.ExpectedProject, binding.ProjectID); err != nil {
		return project.Binding{}, project.Workspace{}, err
	}
	if err := c.require(ctx, binding.ProjectID, req.SenderOpenID, project.RoleWrite); err != nil {
		return project.Binding{}, project.Workspace{}, err
	}
	workspace, err := c.projects.Materialize(ctx, project.Request{Project: binding.ProjectID, Node: selected.Node})
	if err != nil {
		var notHome project.NotHomeError
		if errors.As(err, &notHome) {
			return project.Binding{}, project.Workspace{}, UserError{Text: c.text.T(i18n.ProjectNotHome,
				binding.ProjectID, notHome.PlaceList(), selected.ID, placeLabel(selected.Node), placeLabel(selected.Node), protocol.CommandProject)}
		}
		return project.Binding{}, project.Workspace{}, err
	}
	return binding, workspace, nil
}

// sessionDrifted says whether a saved session was opened under a different
// binding than the one in force now. A session from before bindings were
// recorded is judged on its directory alone.
func sessionDrifted(saved state.Session, binding project.Binding, workspace string) bool {
	if saved.Workspace != "" && saved.Workspace != workspace {
		return true
	}
	if saved.ProjectID == "" {
		return false
	}
	return saved.ProjectID != binding.ProjectID || saved.ProjectVersion != binding.Version
}

// projectCmd shows the conversation's project or switches it. Switching
// archives the conversation's live sessions: they were opened in the old
// project's directory, and a session is bound to exactly one binding.
func (c *Coordinator) projectCmd(ctx context.Context, req Request, rest string) (Result, error) {
	title := c.text.T(i18n.CardProject)
	if c.projects == nil {
		return Result{Title: title, Text: c.text.T(i18n.ProjectsDisabled)}, nil
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return c.projectStatus(ctx, req, title)
	}
	if fields[0] != "use" || len(fields) != 2 {
		return Result{Title: title, Text: c.text.T(i18n.ProjectUsage, protocol.CommandProject)}, nil
	}
	target := fields[1]
	p, ok, err := c.projects.Get(ctx, target)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{Title: title, Text: c.text.T(i18n.ProjectUnknown, target)}, nil
	}
	if err := c.require(ctx, p.ID, req.SenderOpenID, project.RoleRead); err != nil {
		return Result{}, err
	}
	conversation := c.store.Conversation(req.ConversationID)
	for agentID := range conversation.Sessions {
		if c.isActive(req.ConversationID, agentID) {
			return Result{Title: title, Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}, nil
		}
	}
	binding, err := c.projects.Bind(ctx, req.ConversationID, p.ID, req.SenderOpenID)
	if err != nil {
		return Result{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for agentID := range conversation.Sessions {
		if err := c.store.ArchiveSession(req.ConversationID, agentID, now); err != nil {
			log.Printf("turn: archive %s session on project switch: %v", agentID, err)
		}
		// A task belongs to the project it was opened in; the next turn
		// here runs in another. What this conversation still held is
		// closed, so it is not silently continued under a different
		// directory and a different data level.
		c.closeTask(req.ConversationID, agentID)
	}
	log.Printf("turn: conversation %s bound to project %s (v%d) by %s", req.ConversationID, p.ID, binding.Version, req.SenderOpenID)
	return Result{Title: title, Text: c.text.T(i18n.ProjectSwitched, p.ID, homeLabel(p))}, nil
}

func (c *Coordinator) projectStatus(ctx context.Context, req Request, title string) (Result, error) {
	var lines []string
	binding, ok, err := c.projects.Binding(ctx, req.ConversationID)
	if err != nil {
		return Result{}, err
	}
	if ok {
		if p, found, _ := c.projects.Get(ctx, binding.ProjectID); found {
			lines = append(lines, c.text.T(i18n.ProjectCurrent, p.ID, homeLabel(p), binding.Version))
		}
	} else {
		lines = append(lines, c.text.T(i18n.ProjectUnbound, protocol.CommandProject))
	}
	all, err := c.projects.List(ctx)
	if err != nil {
		return Result{}, err
	}
	lines = append(lines, "", c.text.T(i18n.ProjectListHeader))
	for _, p := range all {
		marker := "  "
		if ok && p.ID == binding.ProjectID {
			marker = "* "
		}
		lines = append(lines, fmt.Sprintf("%s%s — %s (%s, %s)", marker, p.ID, homeLabel(p), p.Level, p.Repo))
	}
	return Result{Title: title, Text: strings.Join(lines, "\n")}, nil
}

func (c *Coordinator) isActive(conversationID, agentID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, active := c.active[sessionKey(conversationID, agentID)]
	return active
}

func homeLabel(p project.Project) string {
	return placeLabel(p.Home.Node) + ":" + p.Home.Path
}

func placeLabel(node string) string { return nodewire.Place(node) }
