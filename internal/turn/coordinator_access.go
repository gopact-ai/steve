package turn

import (
	"context"
	"fmt"
	"log"
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

// SetIntents wires the side-effect ledger the /effects verb reads.
func (c *Coordinator) SetIntents(s *intent.Service) { c.intents = s }
