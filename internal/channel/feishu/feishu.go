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
	ChatID       string
	ChatType     string // p2p | group
	MessageID    string
	SenderOpenID string
	Text         string
}

// Handler consumes inbound messages; it must not block the event loop.
type Handler func(msg InboundMessage)

type Channel struct {
	api *lark.Client
	ws  *larkws.Client
}

var mentionToken = regexp.MustCompile(`@_user_\d+\s*`)

func New(appID, appSecret string, handler Handler) *Channel {
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
			msg, ok := normalize(event)
			if !ok {
				return nil
			}
			handler(msg)
			return nil
		})

	return &Channel{
		api: lark.NewClient(appID, appSecret),
		ws: larkws.NewClient(appID, appSecret,
			larkws.WithEventHandler(eventHandler),
			larkws.WithLogLevel(larkcore.LogLevelInfo),
		),
	}
}

// Start blocks and maintains the long connection until ctx is done.
func (c *Channel) Start(ctx context.Context) error {
	return c.ws.Start(ctx)
}

func normalize(event *larkim.P2MessageReceiveV1) (InboundMessage, bool) {
	if event.Event == nil || event.Event.Message == nil {
		return InboundMessage{}, false
	}
	if event.Event.Sender != nil && deref(event.Event.Sender.SenderType) == "bot" {
		return InboundMessage{}, false
	}
	m := event.Event.Message
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
		ChatID:       deref(m.ChatId),
		ChatType:     deref(m.ChatType),
		MessageID:    deref(m.MessageId),
		SenderOpenID: sender,
		Text:         text,
	}, true
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
