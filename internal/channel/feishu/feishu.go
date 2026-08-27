// Package feishu implements the Feishu (Lark) channel using the WebSocket
// long-connection event mode, so no public callback endpoint is required.
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/gopact-ai/steve/internal/protocol"
)

const (
	senderTypeBot          = "bot"
	messageTypeText        = "text"
	messageTypeImage       = "image"
	messageTypePost        = "post"
	messageTypeInteractive = "interactive"
	mentionTypeBot         = "bot"
	maxImageBytes          = 3 << 20
	maxImages              = 4
	maxQuoteRunes          = 800
	maxQuoteDepth          = 5
)

// InboundMessage is a normalized text message from Feishu.
type InboundMessage struct {
	ConversationID string
	ChatID         string
	ChatType       protocol.ChatType
	MessageID      string
	SenderOpenID   string
	Text           string
	Quote          string
	ParentID       string
	ImageKeys      []string
	Images         []Image
	Mentioned      bool
}

type Image struct {
	MIME string
	Data []byte
}

type CardAction struct {
	OpenID    string
	MessageID string
	ChatID    string
	Action    string
	RequestID string
	Decision  string
}

type CardToast struct {
	Type    string
	Content string
}

// Handler consumes inbound messages; it must not block the event loop.
type Handler func(msg InboundMessage)

type Options struct {
	AppID            string
	AppSecret        string
	Domain           string
	Access           Access
	AllowUnmentioned bool
	OnCardAction     func(CardAction) CardToast
}

type longConn interface {
	Start(context.Context) error
	Close()
}

type Channel struct {
	api    *lark.Client
	ws     longConn
	access Access
}

var mentionToken = regexp.MustCompile("@_(user_\\d+|all)[\\s\u200b]*")

func New(ctx context.Context, opts Options, handler Handler) (*Channel, error) {
	api := newAPI(opts.AppID, opts.AppSecret, opts.Domain)
	identity, err := botIdentity(ctx, api)
	if err != nil {
		return nil, err
	}
	channel := &Channel{api: api, access: opts.Access}
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			msg, ok := normalize(event, identity.OpenID, opts.AllowUnmentioned)
			if !ok {
				return nil
			}
			if decide(msg, channel.access) != actionAllow {
				return nil
			}
			dlCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			channel.attachImages(dlCtx, &msg)
			channel.attachQuoted(dlCtx, &msg)
			cancel()
			// Hand off rather than run the turn here. This callback is the
			// connection's event loop: blocking it for the length of a turn
			// means the next message cannot arrive until the current one
			// finishes — which is exactly the message that was meant to
			// interrupt it. Returning immediately also acks the event before
			// Feishu's redelivery window instead of after the agent is done.
			go handler(msg)
			return nil
		}).
		OnP2CardActionTrigger(func(_ context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			if opts.OnCardAction == nil {
				return &callback.CardActionTriggerResponse{}, nil
			}
			toast := opts.OnCardAction(parseCardAction(event))
			resp := &callback.CardActionTriggerResponse{}
			if toast.Content != "" {
				resp.Toast = &callback.Toast{Type: toast.Type, Content: toast.Content}
			}
			return resp, nil
		})

	channel.ws = larkws.NewClient(opts.AppID, opts.AppSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithDomain(BaseURL(opts.Domain)),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)
	return channel, nil
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
	if event.Event.Sender != nil && deref(event.Event.Sender.SenderType) == senderTypeBot {
		return InboundMessage{}, false
	}
	m := event.Event.Message
	chatType := protocol.ParseChatType(deref(m.ChatType))
	mentioned := mentionsBot(m.Mentions, botOpenID)
	if chatType == protocol.ChatGroup && !allowUnmentioned && !mentioned {
		return InboundMessage{}, false
	}
	text, imageKeys, ok := parseContent(deref(m.MessageType), deref(m.Content))
	if !ok {
		log.Printf("feishu: ignoring message type %q", deref(m.MessageType))
		return InboundMessage{}, false
	}
	sender := ""
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil {
		sender = deref(event.Event.Sender.SenderId.OpenId)
	}
	return InboundMessage{
		ConversationID: conversationID(m),
		ChatID:         deref(m.ChatId),
		ChatType:       chatType,
		MessageID:      deref(m.MessageId),
		ParentID:       deref(m.ParentId),
		SenderOpenID:   sender,
		Text:           text,
		ImageKeys:      imageKeys,
		Mentioned:      mentioned || chatType != protocol.ChatGroup,
	}, true
}

func parseContent(messageType, raw string) (string, []string, bool) {
	switch messageType {
	case messageTypeText:
		var content struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &content); err != nil {
			log.Printf("feishu: bad text content: %v", err)
			return "", nil, false
		}
		text := strings.TrimSpace(mentionToken.ReplaceAllString(content.Text, ""))
		return text, nil, text != ""
	case messageTypeImage:
		var content struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(raw), &content); err != nil || content.ImageKey == "" {
			log.Printf("feishu: bad image content: %v", err)
			return "", nil, false
		}
		return "", []string{content.ImageKey}, true
	case messageTypePost:
		return parsePost(raw)
	default:
		return "", nil, false
	}
}

func parsePost(raw string) (string, []string, bool) {
	var content struct {
		Title   string `json:"title"`
		Content [][]struct {
			Tag      string `json:"tag"`
			Text     string `json:"text"`
			ImageKey string `json:"image_key"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &content); err != nil {
		log.Printf("feishu: bad post content: %v", err)
		return "", nil, false
	}
	var text strings.Builder
	if title := strings.TrimSpace(content.Title); title != "" {
		text.WriteString(title)
	}
	var keys []string
	for _, row := range content.Content {
		for _, item := range row {
			switch item.Tag {
			case "img":
				if item.ImageKey != "" && len(keys) < maxImages {
					keys = append(keys, item.ImageKey)
				}
			case "text", "a", "at":
				piece := strings.TrimSpace(mentionToken.ReplaceAllString(item.Text, ""))
				if piece == "" {
					continue
				}
				if text.Len() > 0 {
					text.WriteByte(' ')
				}
				text.WriteString(piece)
			}
		}
	}
	out := strings.TrimSpace(text.String())
	return out, keys, out != "" || len(keys) > 0
}

func mentionsBot(mentions []*larkim.MentionEvent, botOpenID string) bool {
	for _, mention := range mentions {
		if mention != nil && deref(mention.MentionedType) == mentionTypeBot && mention.Id != nil && deref(mention.Id.OpenId) == botOpenID {
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

type Sent struct {
	ChatID    string
	MessageID string
}

// Send creates a new message to receiveID (an open_id). The returned ChatID
// is the p2p conversation future inbound events will use.
func (c *Channel) Send(ctx context.Context, receiveID, text string) (Sent, error) {
	return c.send(ctx, "open_id", receiveID, text)
}

// SendChat creates a new message in a chat. Replies and cards need a real
// message to attach to, which is what the returned MessageID provides.
func (c *Channel) SendChat(ctx context.Context, chatID, text string) (Sent, error) {
	return c.send(ctx, "chat_id", chatID, text)
}

func (c *Channel) send(ctx context.Context, idType, receiveID, text string) (Sent, error) {
	if receiveID == "" {
		return Sent{}, fmt.Errorf("feishu send: receive id is required")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return Sent{}, err
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(idType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType(messageTypeText).
			Content(string(content)).
			Build()).
		Build()
	resp, err := c.api.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return Sent{}, fmt.Errorf("feishu send: %w", err)
	}
	if !resp.Success() {
		return Sent{}, fmt.Errorf("feishu send: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return Sent{}, fmt.Errorf("feishu send: empty response")
	}
	sent := Sent{ChatID: deref(resp.Data.ChatId), MessageID: deref(resp.Data.MessageId)}
	if sent.ChatID == "" {
		return Sent{}, fmt.Errorf("feishu send: empty chat id")
	}
	return sent, nil
}

// ReplyCard replies with a Card 2.0 payload and returns the card message id.
func (c *Channel) ReplyCard(ctx context.Context, messageID string, payload []byte) (string, error) {
	if messageID == "" || len(payload) == 0 {
		return "", fmt.Errorf("feishu card reply: message id and payload are required")
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(messageTypeInteractive).
			Content(string(payload)).
			Build()).
		Build()
	resp, err := c.api.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu card reply: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu card reply: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || deref(resp.Data.MessageId) == "" {
		return "", fmt.Errorf("feishu card reply: empty message id")
	}
	return deref(resp.Data.MessageId), nil
}

// PatchCard updates an existing card in place. Falls back to update if patch fails.
func (c *Channel) PatchCard(ctx context.Context, messageID string, payload []byte) error {
	if messageID == "" || len(payload) == 0 {
		return fmt.Errorf("feishu card patch: message id and payload are required")
	}
	content := string(payload)
	patch := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(content).Build()).
		Build()
	resp, err := c.api.Im.V1.Message.Patch(ctx, patch)
	if err == nil && resp.Success() {
		return nil
	}
	upd := larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType(messageTypeInteractive).
			Content(content).
			Build()).
		Build()
	updated, updErr := c.api.Im.V1.Message.Update(ctx, upd)
	if updErr != nil {
		if err != nil {
			return fmt.Errorf("feishu card patch: %w", err)
		}
		return fmt.Errorf("feishu card update: %w", updErr)
	}
	if !updated.Success() {
		if err != nil {
			return fmt.Errorf("feishu card patch: code=%d msg=%s", resp.Code, resp.Msg)
		}
		return fmt.Errorf("feishu card update: code=%d msg=%s", updated.Code, updated.Msg)
	}
	return nil
}

// DeleteMessage recalls a message the bot sent, used to clear a stale card
// before a retry replaces it.
func (c *Channel) DeleteMessage(ctx context.Context, messageID string) error {
	if messageID == "" {
		return fmt.Errorf("feishu delete: message id is required")
	}
	req := larkim.NewDeleteMessageReqBuilder().MessageId(messageID).Build()
	resp, err := c.api.Im.V1.Message.Delete(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu delete: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu delete: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// Reply sends a plain-text reply to the given message.
func (c *Channel) Reply(ctx context.Context, messageID, text string) error {
	_, err := c.ReplyText(ctx, messageID, text)
	return err
}

// ReplyText sends a plain-text reply and returns the new message's id, so a
// caller that may need to recall the message later can hold on to it.
func (c *Channel) ReplyText(ctx context.Context, messageID, text string) (string, error) {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("feishu reply: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu reply: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return "", nil
	}
	return deref(resp.Data.MessageId), nil
}

// ReplyThread replies to a message and starts a new topic thread on it,
// returning the anchor message's id and the thread's id. The anchor is what
// the turn's card will reply to, so everything that follows lands inside the
// topic instead of the flat chat.
func (c *Channel) ReplyThread(ctx context.Context, messageID, text string) (string, string, error) {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", "", err
	}
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			ReplyInThread(true).
			Build()).
		Build()
	resp, err := c.api.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("feishu thread reply: %w", err)
	}
	if !resp.Success() {
		return "", "", fmt.Errorf("feishu thread reply: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return "", "", fmt.Errorf("feishu thread reply: empty response")
	}
	return deref(resp.Data.MessageId), deref(resp.Data.ThreadId), nil
}

func (c *Channel) attachImages(ctx context.Context, msg *InboundMessage) {
	if c == nil || c.api == nil || msg == nil || len(msg.ImageKeys) == 0 {
		return
	}
	msg.Images = append(msg.Images, c.fetchImages(ctx, msg.MessageID, msg.ImageKeys)...)
}

// attachQuoted walks the reply chain, so "look at this" pointing at a reply
// that itself quoted an image still reaches the agent with that image.
func (c *Channel) attachQuoted(ctx context.Context, msg *InboundMessage) {
	if c == nil || c.api == nil || msg == nil || msg.ParentID == "" {
		return
	}
	seen := map[string]bool{msg.MessageID: true}
	quotes := make([]string, 0, maxQuoteDepth)
	budget := maxQuoteRunes
	for id, depth := msg.ParentID, 0; id != "" && depth < maxQuoteDepth; depth++ {
		if seen[id] {
			break
		}
		seen[id] = true
		parent, ok := c.fetchMessage(ctx, id)
		if !ok {
			break
		}
		// A card or other unsupported type in the middle of the chain is
		// skipped, not treated as the end of it.
		text, keys, _ := parseContent(deref(parent.MsgType), deref(parent.Body.Content))
		if text != "" && budget > 0 {
			quote := truncateRunes(text, budget)
			budget -= len([]rune(quote))
			quotes = append(quotes, quote)
		}
		if len(keys) > 0 && len(msg.Images) < maxImages {
			msg.ImageKeys = append(msg.ImageKeys, keys...)
			msg.Images = append(msg.Images, c.fetchImages(ctx, id, keys)...)
		}
		id = deref(parent.ParentId)
	}
	// Oldest first, so the agent reads the chain in the order it happened.
	for i, j := 0, len(quotes)-1; i < j; i, j = i+1, j-1 {
		quotes[i], quotes[j] = quotes[j], quotes[i]
	}
	msg.Quote = strings.Join(quotes, "\n---\n")
}

func (c *Channel) fetchMessage(ctx context.Context, messageID string) (*larkim.Message, bool) {
	req := larkim.NewGetMessageReqBuilder().MessageId(messageID).Build()
	resp, err := c.api.Im.V1.Message.Get(ctx, req)
	if err != nil {
		log.Printf("feishu: fetch quoted message failed: %v", err)
		return nil, false
	}
	if !resp.Success() {
		log.Printf("feishu: fetch quoted message failed: code=%d msg=%s", resp.Code, resp.Msg)
		return nil, false
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 {
		return nil, false
	}
	parent := resp.Data.Items[0]
	if parent == nil || parent.Body == nil {
		return nil, false
	}
	return parent, true
}

func (c *Channel) fetchImages(ctx context.Context, messageID string, keys []string) []Image {
	if len(keys) > maxImages {
		keys = keys[:maxImages]
	}
	images := make([]Image, 0, len(keys))
	for _, key := range keys {
		img, err := c.downloadImage(ctx, messageID, key)
		if err != nil {
			log.Printf("feishu: download image failed: %v", err)
			continue
		}
		images = append(images, img)
	}
	return images
}

func truncateRunes(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

func (c *Channel) downloadImage(ctx context.Context, messageID, fileKey string) (Image, error) {
	if messageID == "" || fileKey == "" {
		return Image{}, fmt.Errorf("feishu image: message id and file key are required")
	}
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(fileKey).
		Type("image").
		Build()
	resp, err := c.api.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		return Image{}, fmt.Errorf("feishu image: %w", err)
	}
	if !resp.Success() {
		return Image{}, fmt.Errorf("feishu image: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if closer, ok := resp.File.(io.Closer); ok {
		defer closer.Close()
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, maxImageBytes+1))
	if err != nil {
		return Image{}, fmt.Errorf("feishu image: %w", err)
	}
	if len(data) == 0 {
		return Image{}, fmt.Errorf("feishu image: empty file")
	}
	if len(data) > maxImageBytes {
		return Image{}, fmt.Errorf("feishu image: too large")
	}
	return Image{MIME: sniffMIME(resp.FileName, data), Data: data}, nil
}

func sniffMIME(name string, data []byte) string {
	if mime := http.DetectContentType(data); mime != "" && mime != "application/octet-stream" {
		return mime
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func parseCardAction(event *callback.CardActionTriggerEvent) CardAction {
	if event == nil || event.Event == nil {
		return CardAction{}
	}
	action := CardAction{}
	if event.Event.Operator != nil {
		action.OpenID = event.Event.Operator.OpenID
	}
	if event.Event.Context != nil {
		action.MessageID = event.Event.Context.OpenMessageID
		action.ChatID = event.Event.Context.OpenChatID
	}
	if event.Event.Action != nil {
		action.Action = mapString(event.Event.Action.Value, "action")
		action.RequestID = mapString(event.Event.Action.Value, "request_id")
		action.Decision = mapString(event.Event.Action.Value, "decision")
	}
	return action
}

func mapString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
