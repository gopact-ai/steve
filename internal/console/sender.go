package console

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/gopact-ai/steve/internal/agentmcp"
)

// Sender routes an agent's messaging calls: anchors that are Feishu
// messages go to Feishu, console anchors stay on the page.
type Sender struct {
	Feishu  agentmcp.Sender
	Console *Service
}

func (s Sender) isConsole(anchor string) bool { return strings.HasPrefix(anchor, AnchorMark) }

func (s Sender) ReplyCard(ctx context.Context, messageID string, payload []byte) (string, error) {
	if s.isConsole(messageID) {
		return s.Console.Milestone(messageID, cardText(payload)), nil
	}
	return s.Feishu.ReplyCard(ctx, messageID, payload)
}

func (s Sender) ReplyText(ctx context.Context, messageID, text string) (string, error) {
	if s.isConsole(messageID) {
		return s.Console.Milestone(messageID, text), nil
	}
	return s.Feishu.ReplyText(ctx, messageID, text)
}

func (s Sender) PatchCard(ctx context.Context, messageID string, payload []byte) error {
	if s.isConsole(messageID) {
		s.Console.Milestone(messageID, cardText(payload))
		return nil
	}
	return s.Feishu.PatchCard(ctx, messageID, payload)
}

func (s Sender) DeleteMessage(ctx context.Context, messageID string) error {
	if s.isConsole(messageID) {
		return nil
	}
	return s.Feishu.DeleteMessage(ctx, messageID)
}

// cardText pulls the markdown out of a card payload, best effort; the raw
// payload is shown when the shape is unknown.
func cardText(payload []byte) string {
	var card struct {
		Body struct {
			Elements []struct {
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
		Elements []struct {
			Content string `json:"content"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(payload, &card); err == nil {
		var parts []string
		for _, e := range append(card.Body.Elements, card.Elements...) {
			if e.Content != "" {
				parts = append(parts, e.Content)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return string(payload)
}
