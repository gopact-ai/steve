package turn

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
)

// historyCmd lists the sessions /clear archived, and puts one back. Clearing
// ends the agent's context but leaves its session on the agent's side, so an
// archived record is a live handle rather than a receipt.
func (c *Coordinator) historyCmd(req Request, selected agent.Agent, rest string) (Result, error) {
	conversationID := req.ConversationID
	archived := c.store.ArchivedSessions(conversationID, selected.ID)
	rest = strings.TrimSpace(rest)
	if rest == "" {
		if len(archived) == 0 {
			return Result{AgentID: selected.ID, Text: c.text.T(i18n.HistoryEmpty, selected.ID)}, nil
		}
		return Result{AgentID: selected.ID, Text: c.text.T(
			i18n.HistoryList, selected.ID, historyList(archived), protocol.CommandHistory)}, nil
	}
	index, err := strconv.Atoi(rest)
	if err != nil {
		return Result{}, UserError{Text: c.text.T(i18n.HistoryUnknown, rest)}
	}
	// A restore rewrites which session the next turn uses, so refuse while
	// one is running rather than swapping it mid-flight.
	c.mu.Lock()
	busy := c.cancels[sessionKey(conversationID, selected.ID)] != nil
	c.mu.Unlock()
	if busy {
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	restored, err := c.store.RestoreSession(conversationID, selected.ID, index)
	if err != nil {
		return Result{}, UserError{Text: c.text.T(i18n.HistoryUnknown, rest)}
	}
	return Result{AgentID: selected.ID, Text: c.text.T(
		i18n.HistoryRestored, selected.ID, index, shortID(restored.UpstreamID))}, nil
}

// historyList numbers the archive newest first, which is the order someone
// reaching for "the one I just cleared" expects. The number is bracketed
// rather than written "1." because Feishu reads a leading "1." as an ordered
// list and swallows the line that follows the block into the list item.
func historyList(archived []state.Archived) string {
	lines := make([]string, 0, len(archived))
	for i, item := range archived {
		lines = append(lines, fmt.Sprintf("[%d] %s · %s", i+1, historyWhen(item.ArchivedAt), shortID(item.UpstreamID)))
	}
	return strings.Join(lines, "\n")
}

func historyWhen(at string) string {
	parsed, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return at
	}
	return parsed.Local().Format("01-02 15:04")
}

// shortID keeps a session id recognisable without pasting a whole UUID.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:8] + "…"
}
