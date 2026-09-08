// The /project command: binding a conversation to a project and
// reporting where it stands.

package turn

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"log/slog"
	"strings"
	"time"
)

// projectCmd shows the conversation's project or switches it. Switching
// archives the conversation's live sessions: they were opened in the old
// project's directory, and a session is bound to exactly one binding.
func (c commands) projectCmd(ctx context.Context, req Request, rest string) (Result, error) {
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
			slog.Error(fmt.Sprintf("turn: archive %s session on project switch: %v", agentID, err), "conversation", req.ConversationID, "agent", agentID, "project", p.ID)
		}
		// A task belongs to the project it was opened in; the next turn
		// here runs in another. What this conversation still held is
		// closed, so it is not silently continued under a different
		// directory and a different data level.
		c.closeTask(req.ConversationID, agentID)
	}
	slog.Info(fmt.Sprintf("turn: conversation %s bound to project %s (v%d) by %s", req.ConversationID, p.ID, binding.Version, req.SenderOpenID), "conversation", req.ConversationID, "project", p.ID)
	return Result{Title: title, Text: c.text.T(i18n.ProjectSwitched, p.ID, homeLabel(p))}, nil
}

func (c commands) projectStatus(ctx context.Context, req Request, title string) (Result, error) {
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

func homeLabel(p project.Project) string {
	return placeLabel(p.Home.Node) + ":" + p.Home.Path
}

func placeLabel(node string) string { return nodewire.Place(node) }
