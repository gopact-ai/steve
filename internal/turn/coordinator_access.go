package turn

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

// require refuses a principal whose role in the project is below want.
func (c *Coordinator) require(ctx context.Context, projectID, principal string, want project.Role) error {
	if c.projects == nil {
		return nil
	}
	role, err := c.projects.Access(ctx, projectID, principal, c.ownerOpenID)
	if err != nil {
		return err
	}
	if !role.AtLeast(want) {
		return UserError{Text: c.text.T(i18n.ProjectAccess, projectID, role, want)}
	}
	return nil
}

func (c *Coordinator) isOwner(req Request) bool {
	return c.ownerOpenID != "" && req.SenderOpenID == c.ownerOpenID
}

// grantCmd: `/grant <project>` lists; `/grant <project> <open_id> <role>`
// grants, for the owner or a project admin.
func (c *Coordinator) grantCmd(ctx context.Context, req Request, rest string) (Result, error) {
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

// held is an answer from a sealed project waiting for the owner. It lives
// only here: the hub is not a place sealed content is kept.
type held struct {
	id     string
	req    Request
	task   string
	text   string
	result Result
	at     time.Time
}

var disclosuresMu sync.Mutex

// gateDisclosure holds back the answer of a turn on a sealed project until
// the owner approves it leaving. The requester gets the id; the content
// waits in memory.
func (c *Coordinator) gateDisclosure(ctx context.Context, req Request, result Result) (Result, error) {
	if c.projects == nil || result.Text == "" {
		return result, nil
	}
	binding, ok, err := c.projects.Binding(ctx, req.ConversationID)
	if err != nil || !ok {
		return result, nil
	}
	p, ok, err := c.projects.Get(ctx, binding.ProjectID)
	if err != nil || !ok || p.Level != project.LevelSealed {
		return result, nil
	}
	if c.isOwner(req) {
		return result, nil // the owner is who approves; asking them to approve their own answer is theatre
	}
	attemptID, taskID := "", ""
	if c.attempts != nil {
		if r, found, _ := c.attempts.LatestForTurn(ctx, req.MessageID); found {
			attemptID, taskID = r.ID, r.TaskID
		}
	}
	id := fmt.Sprintf("disc-%d", time.Now().UnixNano()%1_000_000_007)
	h := held{id: id, req: req, task: taskID, text: result.Text, result: result, at: time.Now()}
	if err := c.projects.ProposeDisclosure(ctx, project.DisclosureRequest{
		ID: id, Project: p.ID, TaskID: taskID, Attempt: attemptID, ConversationID: req.ConversationID,
		Requester: req.SenderOpenID, Bytes: len(result.Text),
	}); err != nil {
		return Result{}, err
	}
	disclosuresMu.Lock()
	if c.disclosures == nil {
		c.disclosures = map[string]held{}
	}
	c.disclosures[id] = h
	disclosuresMu.Unlock()
	log.Printf("turn: sealed answer for %s held as disclosure %s (%d chars)", req.ConversationID, id, len(result.Text))
	return Result{AgentID: result.AgentID, Title: c.text.T(i18n.CardDisclosure),
		Text: c.text.T(i18n.DisclosurePending, len([]rune(result.Text)), p.ID, protocol.CommandApprove, id)}, nil
}

// decideCmd is /approve and /deny: owner only.
func (c *Coordinator) decideCmd(ctx context.Context, req Request, cmd protocol.Command, rest string) (Result, error) {
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

// SetIntents wires the side-effect ledger the /effects verb reads.
func (c *Coordinator) SetIntents(s *intent.Service) { c.intents = s }

// effectsCmd lists outcome-unknown side effects, or resolves one.
func (c *Coordinator) effectsCmd(ctx context.Context, req Request, rest string) (Result, error) {
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
