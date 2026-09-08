// The /grant, /approve, /deny and /effects commands: the owner deciding
// who may see a sealed project and what an uncertain effect really did.

package turn

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"strings"
)

// grantCmd: `/grant <project>` lists; `/grant <project> <open_id> <role>`
// grants, for the owner or a project admin.
func (c commands) grantCmd(ctx context.Context, req Request, rest string) (Result, error) {
	title := c.text.T(i18n.CardProject)
	if c.projects == nil {
		return Result{Title: title, Text: c.text.T(i18n.ProjectsDisabled)}, nil
	}
	fields := strings.Fields(rest)
	switch len(fields) {
	case 1:
		p, ok, err := c.projects.Get(ctx, fields[0])
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{Title: title, Text: c.text.T(i18n.ProjectUnknown, fields[0])}, nil
		}
		fallback, _ := c.projects.Access(ctx, p.ID, "", "")
		grants, err := c.projects.Grants(ctx, p.ID)
		if err != nil {
			return Result{}, err
		}
		lines := []string{c.text.T(i18n.GrantHeader, p.ID, fallback)}
		for _, g := range grants {
			lines = append(lines, fmt.Sprintf("  %s — %s (by %s)", g.Principal, g.Role, g.By))
		}
		return Result{Title: title, Text: strings.Join(lines, "\n")}, nil
	case 3:
		if err := c.require(ctx, fields[0], req.SenderOpenID, project.RoleAdmin); err != nil {
			return Result{}, err
		}
		g, err := c.projects.Grant(ctx, fields[0], fields[1], project.Role(fields[2]), req.SenderOpenID)
		if err != nil {
			var user UserError
			if _, isUser := err.(UserError); isUser {
				return Result{}, user
			}
			return Result{Title: title, Text: err.Error()}, nil
		}
		return Result{Title: title, Text: c.text.T(i18n.GrantDone, g.Principal, g.Project, g.Role)}, nil
	}
	return Result{Title: title, Text: c.text.T(i18n.GrantUsage, protocol.CommandGrant, protocol.CommandGrant)}, nil
}

// decideCmd is /approve and /deny: owner only.
func (c commands) decideCmd(ctx context.Context, req Request, cmd protocol.Command, rest string) (Result, error) {
	title := c.text.T(i18n.CardDisclosure)
	if !c.isOwner(req) {
		return Result{Title: title, Text: c.text.T(i18n.OwnerOnly)}, nil
	}
	id := strings.TrimSpace(rest)
	if id == "" {
		return Result{Title: title, Text: c.text.T(i18n.DisclosureUsage, cmd)}, nil
	}
	disclosuresMu.Lock()
	h, ok := c.disclosures[id]
	if ok {
		delete(c.disclosures, id)
	}
	disclosuresMu.Unlock()
	if !ok {
		return Result{Title: title, Text: c.text.T(i18n.DisclosureUnknown, id)}, nil
	}
	approved := cmd == protocol.CommandApprove
	if err := c.projects.ResolveDisclosure(ctx, id, approved, req.SenderOpenID); err != nil {
		return Result{}, err
	}
	if !approved {
		return Result{Title: title, Text: c.text.T(i18n.DisclosureDenied, id)}, nil
	}
	if c.notifier != nil {
		c.notifier(TaskNotice{TaskID: h.task, ChatID: h.req.ChatID, MessageID: h.req.MessageID, Requester: h.req.SenderOpenID, Conversation: h.req.ConversationID, Text: h.text})
	}
	return Result{Title: title, Text: c.text.T(i18n.DisclosureApproved, id)}, nil
}

// effectsCmd lists outcome-unknown side effects, or resolves one.
func (c commands) effectsCmd(ctx context.Context, req Request, rest string) (Result, error) {
	title := c.text.T(i18n.CardEffects)
	fields := strings.Fields(rest)
	if c.intents == nil {
		return Result{Title: title, Text: c.text.T(i18n.EffectsNone)}, nil
	}
	switch len(fields) {
	case 0:
		unresolved, err := c.intents.Unresolved(ctx)
		if err != nil {
			return Result{}, err
		}
		if len(unresolved) == 0 {
			return Result{Title: title, Text: c.text.T(i18n.EffectsNone)}, nil
		}
		lines := []string{c.text.T(i18n.EffectsHeader, protocol.CommandEffects)}
		for _, it := range unresolved {
			lines = append(lines, fmt.Sprintf("  %s — %s by attempt %s of task #%s at %s: %s", it.ID, it.Tool, it.AttemptID, it.TaskID, it.At.Format("15:04:05"), it.Error))
		}
		return Result{Title: title, Text: strings.Join(lines, "\n")}, nil
	case 2:
		if !c.isOwner(req) {
			return Result{Title: title, Text: c.text.T(i18n.OwnerOnly)}, nil
		}
		it, err := c.intents.Resolve(ctx, fields[0], fields[1], req.SenderOpenID)
		if err != nil {
			return Result{Title: title, Text: err.Error()}, nil
		}
		return Result{Title: title, Text: c.text.T(i18n.EffectsResolved, it.ID, it.State)}, nil
	}
	return Result{Title: title, Text: c.text.T(i18n.EffectsUsage, protocol.CommandEffects)}, nil
}
