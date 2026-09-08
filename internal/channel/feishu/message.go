package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/text"
)

// MessageAPI is the provider boundary used for agent-authored milestones.
// Channel implements it; conversation ownership is checked before it is called.
type MessageAPI interface {
	ReplyCard(context.Context, string, []byte) (string, error)
	ReplyText(context.Context, string, string) (string, error)
	PatchCard(context.Context, string, []byte) error
	DeleteMessage(context.Context, string) error
}

// Messenger renders neutral messages for Feishu. Provider card schemas and
// mention syntax stay here, outside the agent's collaboration tools.
type Messenger struct {
	API MessageAPI
}

var _ channel.Messenger = Messenger{}
var _ MessageAPI = (*Channel)(nil)

func (m Messenger) Send(ctx context.Context, to channel.Address, msg channel.Message) (string, error) {
	if err := m.validateAddress(to); err != nil {
		return "", err
	}
	content, format, err := milestoneContent(msg)
	if err != nil {
		return "", err
	}
	if format == "text" {
		id, err := m.API.ReplyText(ctx, to.Message, content)
		return id, messageOutcome(err)
	}
	id, err := m.API.ReplyCard(ctx, to.Message, milestoneCard(content, stripMentions(msg.Attribution)))
	return id, messageOutcome(err)
}

func (m Messenger) Update(ctx context.Context, to channel.Address, msg channel.Message) error {
	if err := m.validateAddress(to); err != nil {
		return err
	}
	content, format, err := milestoneContent(msg)
	if err != nil {
		return err
	}
	if format != "markdown" {
		return errors.New("feishu: only markdown messages can be updated; recall and resend a text message")
	}
	return messageOutcome(m.API.PatchCard(ctx, to.Message, milestoneCard(content, stripMentions(msg.Attribution))))
}

func (m Messenger) Recall(ctx context.Context, to channel.Address) error {
	if err := m.validateAddress(to); err != nil {
		return err
	}
	return messageOutcome(m.API.DeleteMessage(ctx, to.Message))
}

// Once a provider request has started, a broken transport cannot establish
// whether it took effect. Preserve that uncertainty for the intent journal;
// explicit API refusals and local validation errors remain definite failures.
func messageOutcome(err error) error {
	if err == nil {
		return nil
	}
	var networkError net.Error
	if errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(channel.ErrOutcomeUnknown, err)
	}
	return err
}

func (m Messenger) validateAddress(to channel.Address) error {
	if to.Channel != "feishu" {
		return errors.New("feishu: destination belongs to another channel")
	}
	if strings.TrimSpace(to.Conversation) == "" || strings.TrimSpace(to.Message) == "" {
		return errors.New("feishu: conversation and message are required")
	}
	if m.API == nil {
		return errors.New("feishu: messaging is not configured")
	}
	return nil
}

const (
	maxMilestoneRunes        = 6000
	maxMilestoneSummaryRunes = 100
)

func milestoneContent(msg channel.Message) (string, string, error) {
	format := msg.Format
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "text" {
		return "", "", fmt.Errorf("feishu: unsupported message format %q", format)
	}
	content := text.Clip(stripMentions(msg.Content), maxMilestoneRunes)
	if strings.TrimSpace(content) == "" {
		return "", "", errors.New("feishu: message content is required")
	}
	return content, format, nil
}

// Agent-authored milestones never notify people. Only the platform's final
// answer decides whom to mention, for both text and card markdown.
var atTag = regexp.MustCompile(`(?i)<\s*/?\s*at\b[^>]*>`)

func stripMentions(s string) string {
	return atTag.ReplaceAllString(s, "")
}

// milestoneCard is a minimal Card 2.0 with the same identity tail as the
// platform's final card, including the stage badge supplied by the caller.
func milestoneCard(markdown, tail string) []byte {
	elements := []map[string]any{{
		"tag": "markdown", "content": markdown, "margin": "0px 0px 8px 0px",
	}}
	if tail != "" {
		elements = append(elements, map[string]any{
			"tag": "markdown", "text_size": "notation",
			"content": "<font color='grey'>" + tail + "</font>", "margin": "0px",
		})
	}
	out := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true, "width_mode": "default",
			"summary": map[string]any{"content": text.Clip(strings.Join(strings.Fields(markdown), " "), maxMilestoneSummaryRunes)},
		},
		"body": map[string]any{
			"direction": "vertical", "padding": "12px", "elements": elements,
		},
	}
	// All values are strings, booleans, maps and slices, so encoding cannot fail.
	raw, _ := json.Marshal(out)
	return raw
}
