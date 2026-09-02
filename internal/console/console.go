// Package console is the owner acting from the web page: the same verbs
// the chat has, through the same coordinator, as the same principal. A
// console conversation is "console:<name>"; its anchors are not Feishu
// messages, so what would have been a card or a milestone in the chat is
// kept here and pushed to the page over the change stream.
package console

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
)

// Prefix marks a console conversation and its message ids.
const (
	Prefix     = "console:"
	ChatID     = "console"
	AnchorMark = "web-"
	keep       = 200
)

// Handler is the coordinator's door.
type Handler interface {
	Handle(ctx context.Context, req turn.Request) (turn.Result, error)
}

type Service struct {
	handler Handler
	owner   string
	model   *readmodel.Model

	mu      sync.Mutex
	replies map[string][]readmodel.Reply
}

func New(handler Handler, owner string, model *readmodel.Model) *Service {
	return &Service{handler: handler, owner: owner, model: model, replies: map[string][]readmodel.Reply{}}
}

// IsConsole says whether an anchor or conversation belongs to the page.
func IsConsole(conversationOrAnchor string) bool {
	return strings.HasPrefix(conversationOrAnchor, Prefix) || strings.HasPrefix(conversationOrAnchor, AnchorMark)
}

// Send runs one line as the owner and records the answer.
func (s *Service) Send(ctx context.Context, conversation, input string) (readmodel.Reply, error) {
	if s.owner == "" {
		return readmodel.Reply{}, fmt.Errorf("the console needs feishu.owner_open_id: it acts as the owner")
	}
	if !strings.HasPrefix(conversation, Prefix) {
		conversation = Prefix + conversation
	}
	id := fmt.Sprintf("%s%d", AnchorMark, time.Now().UnixNano())
	s.record(readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, Input: input, Kind: "sent"})
	result, err := s.handler.Handle(ctx, turn.Request{
		ConversationID: conversation, ChatID: ChatID, MessageID: id, Input: input,
		SenderOpenID: s.owner, ChatType: protocol.ChatP2P, Mentioned: true,
	})
	reply := readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, Title: result.Title, Text: result.Text, Kind: "reply"}
	if err != nil {
		reply.Error = err.Error()
		if reply.Text == "" {
			reply.Text = err.Error()
		}
	}
	s.record(reply)
	return reply, err
}

// Replies is a conversation's recent exchanges, oldest first.
func (s *Service) Replies(conversation string) []readmodel.Reply {
	if !strings.HasPrefix(conversation, Prefix) {
		conversation = Prefix + conversation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]readmodel.Reply{}, s.replies[conversation]...)
}

// Notice takes a task notice whose anchor is the console — a resumed
// plan's outcome, an approved disclosure — and shows it on the page.
func (s *Service) Notice(n turn.TaskNotice) {
	conversation := Prefix + "main"
	s.record(readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, Title: "task #" + n.TaskID, Text: n.Text, Kind: "notice"})
}

// Milestone is what an agent's feishu_send becomes on the console.
func (s *Service) Milestone(anchor, text string) string {
	conversation := Prefix + "main"
	id := fmt.Sprintf("%s%d", AnchorMark, time.Now().UnixNano())
	s.record(readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, Text: text, Kind: "milestone"})
	return id
}

func (s *Service) record(r readmodel.Reply) {
	s.mu.Lock()
	list := append(s.replies[r.Conversation], r)
	if len(list) > keep {
		list = list[len(list)-keep:]
	}
	s.replies[r.Conversation] = list
	s.mu.Unlock()
	if s.model != nil {
		text := r.Text
		if r.Kind == "sent" {
			text = r.Input
		}
		s.model.Publish(readmodel.Event{At: r.At, Kind: "console." + r.Kind, Conversation: r.Conversation, Text: text, Title: r.Title})
	}
}
