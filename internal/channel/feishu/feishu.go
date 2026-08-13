// Package feishu implements the Feishu (Lark) channel using the WebSocket
// long-connection event mode, so no public callback endpoint is required.
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// InboundMessage is a normalized text message from Feishu.
type InboundMessage struct {
	ConversationID string
	ChatID         string
	ChatType       string // p2p | group
	MessageID      string
	SenderOpenID   string
	Text           string
}

// Handler consumes inbound messages; it must not block the event loop.
type Handler func(msg InboundMessage)

type Pairing interface {
	RequestPairing(openID string) (code string, err error)
}

type Options struct {
	AppID            string
	AppSecret        string
	Domain           string
	Access           Access
	Pairing          Pairing
	ExtraAllows      func(string) bool
	AllowUnmentioned bool
}

type longConn interface {
	Start(context.Context) error
	Close()
}

type Channel struct {
	api         *lark.Client
	ws          longConn
	access      Access
	pairing     Pairing
	extraAllows func(string) bool
}

var mentionToken = regexp.MustCompile("@_(user_\\d+|all)[\\s\u200b]*")

func New(ctx context.Context, opts Options, handler Handler) (*Channel, error) {
	api := newAPI(opts.AppID, opts.AppSecret, opts.Domain)
	identity, err := botIdentity(ctx, api)
	if err != nil {
		return nil, err
	}
	channel := &Channel{api: api, access: opts.Access, pairing: opts.Pairing, extraAllows: opts.ExtraAllows}
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			msg, ok := normalize(event, identity.OpenID, opts.AllowUnmentioned)
			if !ok {
				return nil
			}
			switch decide(msg, channel.access, channel.extraAllows) {
			case actionAllow:
				handler(msg)
			case actionPairing:
				channel.offerPairing(ctx, msg)
			}
			return nil
		})

	channel.ws = larkws.NewClient(opts.AppID, opts.AppSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithDomain(BaseURL(opts.Domain)),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)
	return channel, nil
}

func (c *Channel) offerPairing(ctx context.Context, msg InboundMessage) {
	if c.pairing == nil {
		return
	}
	code, err := c.pairing.RequestPairing(msg.SenderOpenID)
	if err != nil {
		log.Printf("feishu: pairing request failed: %v", err)
		return
	}
	text := "配对码：" + code + "。在运行 Steve 的机器上执行：steve pairing approve " + code
	if err := c.Reply(ctx, msg.MessageID, text); err != nil {
		log.Printf("feishu: pairing reply failed: %v", err)
	}
}

// Start blocks and maintains the long connection until ctx is done.
// The official WS client ends in select{} and ignores cancellation, so we
// run it in the background and Close it when ctx is canceled.
func (c *Channel) Start(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		done <- c.ws.Start(ctx)
	}()
	select {
	case err := <-done:
		if ctx.Err() != nil {
			c.ws.Close()
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
		c.ws.Close()
		return ctx.Err()
	}
}

func normalize(event *larkim.P2MessageReceiveV1, botOpenID string, allowUnmentioned bool) (InboundMessage, bool) {
	if event.Event == nil || event.Event.Message == nil {
		return InboundMessage{}, false
	}
	if event.Event.Sender != nil && deref(event.Event.Sender.SenderType) == "bot" {
		return InboundMessage{}, false
	}
	m := event.Event.Message
	if deref(m.ChatType) == "group" && !allowUnmentioned && !mentionsBot(m.Mentions, botOpenID) {
		return InboundMessage{}, false
	}
	if deref(m.MessageType) != "text" {
		log.Printf("feishu: ignoring message type %q", deref(m.MessageType))
		return InboundMessage{}, false
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(deref(m.Content)), &content); err != nil {
		log.Printf("feishu: bad text content: %v", err)
		return InboundMessage{}, false
	}
	text := strings.TrimSpace(mentionToken.ReplaceAllString(content.Text, ""))
	if text == "" {
		return InboundMessage{}, false
	}
	sender := ""
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil {
		sender = deref(event.Event.Sender.SenderId.OpenId)
	}
	return InboundMessage{
		ConversationID: conversationID(m),
		ChatID:         deref(m.ChatId),
		ChatType:       deref(m.ChatType),
		MessageID:      deref(m.MessageId),
		SenderOpenID:   sender,
		Text:           text,
	}, true
}

func mentionsBot(mentions []*larkim.MentionEvent, botOpenID string) bool {
	for _, mention := range mentions {
		if mention != nil && deref(mention.MentionedType) == "bot" && mention.Id != nil && deref(mention.Id.OpenId) == botOpenID {
			return true
		}
	}
	return false
}

func conversationID(message *larkim.EventMessage) string {
	if threadID := deref(message.ThreadId); threadID != "" {
		return threadID
	}
	return deref(message.ChatId)
}

// AddReaction puts an emoji reaction on the message and returns its reaction id.
func (c *Channel) AddReaction(ctx context.Context, messageID, emoji string) (string, error) {
	if messageID == "" || emoji == "" {
		return "", fmt.Errorf("feishu reaction: message id and emoji are required")
	}
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emoji).Build()).
			Build()).
		Build()
	resp, err := c.api.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu reaction: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu reaction: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.ReactionId == nil || *resp.Data.ReactionId == "" {
		return "", fmt.Errorf("feishu reaction: empty reaction id")
	}
	return *resp.Data.ReactionId, nil
}

// RemoveReaction deletes a reaction previously added by AddReaction.
func (c *Channel) RemoveReaction(ctx context.Context, messageID, reactionID string) error {
	if messageID == "" || reactionID == "" {
		return fmt.Errorf("feishu reaction delete: message id and reaction id are required")
	}
	req := larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(messageID).
		ReactionId(reactionID).
		Build()
	resp, err := c.api.Im.V1.MessageReaction.Delete(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu reaction delete: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu reaction delete: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// Reply sends a plain-text reply to the given message.
func (c *Channel) Reply(ctx context.Context, messageID, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	resp, err := c.api.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu reply: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu reply: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
