// Package console is the owner acting from the web page: the same verbs
// the chat has, through the same coordinator, as the same principal. A
// console conversation is "console:<name>"; its anchors are not Feishu
// messages, so what would have been a card or a milestone in the chat is
// kept here and pushed to the page over the change stream.
package console

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
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
	// doc keeps the transcript across restarts. A console whose history
	// vanishes with the process would make every restart look like the
	// owner had never said anything.
	doc ledger.Doc
}

func New(handler Handler, owner string, model *readmodel.Model) *Service {
	return &Service{handler: handler, owner: owner, model: model, replies: map[string][]readmodel.Reply{}}
}

// Persist keeps the transcript in a durable document and loads what an
// earlier process left there.
func (s *Service) Persist(doc ledger.Doc) error {
	raw, ok, err := doc.Load()
	if err != nil {
		return fmt.Errorf("console: load transcript: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok && len(raw) > 0 {
		var saved map[string][]readmodel.Reply
		if err := json.Unmarshal(raw, &saved); err != nil {
			return fmt.Errorf("console: transcript is not readable: %w", err)
		}
		for conversation, list := range saved {
			s.replies[conversation] = append(list, s.replies[conversation]...)
		}
	}
	s.doc = doc
	return nil
}

// Conversations lists every console conversation with a transcript.
func (s *Service) Conversations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.replies))
	for name := range s.replies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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
	if s.doc != nil {
		// The whole transcript is small (keep entries per conversation);
		// one durable replace is simpler than a log to compact.
		if raw, err := json.Marshal(s.replies); err == nil {
			if err := s.doc.Save(raw); err != nil {
				log.Printf("console: save transcript: %v", err)
			}
		}
	}
	s.mu.Unlock()
	if s.model != nil {
		text := r.Text
		if r.Kind == "sent" {
			text = r.Input
		}
		s.model.Publish(readmodel.Event{At: r.At, Kind: "console." + r.Kind, Conversation: r.Conversation, Text: text, Title: r.Title})
	}
}
