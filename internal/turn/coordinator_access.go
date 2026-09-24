package turn

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
)

// require refuses a principal whose role in the project is below want.
func (c *Coordinator) require(ctx context.Context, projectID, principal string, want project.Role) error {
	return requireRole(ctx, c.projects, c.text, c.ownerOpenID, projectID, principal, want)
}

// requireRole refuses a principal whose role in the project, with owner as
// the owner identity, is below want; text words the refusal.
func requireRole(ctx context.Context, projects *project.Store, text i18n.Catalog, owner, projectID, principal string, want project.Role) error {
	role, err := projects.Access(ctx, projectID, principal, owner)
	if err != nil {
		return err
	}
	if !role.AtLeast(want) {
		return UserError{Text: text.T(i18n.ProjectAccess, projectID, role, want)}
	}
	return nil
}

func (c *Coordinator) isOwner(req Request) bool {
	return c.ownerOpenID != "" && req.SenderOpenID == c.ownerOpenID
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

// heldDisclosures are a coordinator's held answers by disclosure id,
// guarded by mu.
type heldDisclosures struct {
	mu   sync.Mutex
	byID map[string]held
}

// gateDisclosure holds back the answer of a turn on a sealed project until
// the owner approves it leaving. The requester gets the id; the content
// waits in memory.
func (c *Coordinator) gateDisclosure(ctx context.Context, req Request, result Result) (Result, error) {
	if result.Text == "" {
		return result, nil
	}
	binding, ok, err := c.projects.Binding(ctx, req.ConversationID)
	if err != nil || !ok {
		return result, nil
	}
	p, ok, err := c.projects.Get(ctx, binding.ProjectID)
	if err != nil || !ok || p.Level != datalevel.Sealed {
		return result, nil
	}
	if c.isOwner(req) {
		return result, nil // the owner is who approves; asking them to approve their own answer is theatre
	}
	attemptID, taskID := "", ""
	if r, found, _ := c.attempts.LatestForTurn(ctx, req.MessageID); found {
		attemptID, taskID = r.ID, r.TaskID
	}
	id := fmt.Sprintf("disc-%d", time.Now().UnixNano()%1_000_000_007)
	h := held{id: id, req: req, task: taskID, text: result.Text, result: result, at: time.Now()}
	if err := c.projects.ProposeDisclosure(ctx, project.DisclosureRequest{
		ID: id, Project: p.ID, TaskID: taskID, Attempt: attemptID, ConversationID: req.ConversationID,
		Requester: req.SenderOpenID, Bytes: len(result.Text),
	}); err != nil {
		return Result{}, err
	}
	c.disclosures.mu.Lock()
	if c.disclosures.byID == nil {
		c.disclosures.byID = map[string]held{}
	}
	c.disclosures.byID[id] = h
	c.disclosures.mu.Unlock()
	slog.Info(fmt.Sprintf("turn: sealed answer for %s held as disclosure %s (%d chars)", req.ConversationID, id, len(result.Text)), "conversation", req.ConversationID, "project", p.ID, "attempt", result.Attempt)
	return Result{AgentID: result.AgentID, Attempt: result.Attempt, Title: c.text.T(i18n.CardDisclosure),
		Text: c.text.T(i18n.DisclosurePending, len([]rune(result.Text)), p.ID, protocol.CommandApprove, id)}, nil
}
