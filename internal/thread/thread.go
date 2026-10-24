// Package thread is the conversation: the owner's messages, Steve's replies
// and the notices tasks leave behind. A message is accepted once, by the
// client's nonce; accepting it is not answering it, and the answer arrives
// as another message on the feed.
package thread

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/feed"
	"github.com/gopact-ai/steve/internal/ledger"
)

const (
	RoleOwner  = "owner"
	RoleSteve  = "steve"
	RoleNotice = "notice" // written by the program, not by a model: a task finished, failed or was stopped

	Main = "main"

	EventMessage = "message"
)

// Message is one entry in a thread.
type Message struct {
	ID          string    `json:"id"`
	Thread      string    `json:"thread"`
	Role        string    `json:"role"`
	Text        string    `json:"text"`
	ClientNonce string    `json:"client_nonce,omitempty"`
	TaskID      string    `json:"task_id,omitempty"`
	Tasks       []string  `json:"tasks,omitempty"` // tasks this message started
	At          time.Time `json:"at"`
}

var ErrInvalid = errors.New("thread: invalid message")

const (
	messageKind = "thread-message"
	nonceKind   = "thread-nonce"
	counterKind = "thread-counter"
	maxText     = 32 << 10
)

type Service struct {
	book *ledger.Ledger
	feed *feed.Feed
	now  func() time.Time

	mu        sync.Mutex
	listeners []func(Message)
}

func New(book *ledger.Ledger, events *feed.Feed) *Service {
	return &Service{book: book, feed: events, now: time.Now}
}

// Book is the ledger the thread is kept in.
func (s *Service) Book() *ledger.Ledger { return s.book }

// OnPosted registers a listener told of every message after it is stored.
func (s *Service) OnPosted(f func(Message)) {
	s.mu.Lock()
	s.listeners = append(s.listeners, f)
	s.mu.Unlock()
}

// Post stores a message. With a nonce it is idempotent: the same nonce
// returns the message stored the first time, and created is false.
func (s *Service) Post(ctx context.Context, m Message) (stored Message, created bool, err error) {
	m.Text = strings.TrimSpace(m.Text)
	if m.Thread == "" {
		m.Thread = Main
	}
	switch {
	case m.Role != RoleOwner && m.Role != RoleSteve && m.Role != RoleNotice:
		return Message{}, false, fmt.Errorf("%w: role %q", ErrInvalid, m.Role)
	case m.Text == "" || len(m.Text) > maxText:
		return Message{}, false, fmt.Errorf("%w: text must be 1..%d bytes", ErrInvalid, maxText)
	case len(m.ClientNonce) > 200:
		return Message{}, false, fmt.Errorf("%w: nonce too long", ErrInvalid)
	}
	err = s.book.Update(ctx, func(tx *ledger.Tx) error {
		if m.ClientNonce != "" {
			id, found, err := durable.Get[string](tx, nonceKind, durable.Key(m.Thread, m.ClientNonce))
			if err != nil {
				return err
			}
			if found {
				stored, _, err = durable.Get[Message](tx, messageKind, id)
				return err
			}
		}
		n, _, err := durable.Get[uint64](tx, counterKind, "messages")
		if err != nil {
			return err
		}
		n++
		if err := tx.PutBinding(counterKind, "messages", n); err != nil {
			return err
		}
		m.ID = fmt.Sprintf("%012d", n)
		m.At = s.now().UTC()
		if m.ClientNonce != "" {
			if err := tx.PutBinding(nonceKind, durable.Key(m.Thread, m.ClientNonce), m.ID); err != nil {
				return err
			}
		}
		stored, created = m, true
		return tx.PutBinding(messageKind, m.ID, m)
	})
	if err != nil || !created {
		return stored, false, err
	}
	if s.feed != nil {
		_, _ = s.feed.Publish(EventMessage, stored)
	}
	s.mu.Lock()
	listeners := append([]func(Message){}, s.listeners...)
	s.mu.Unlock()
	for _, f := range listeners {
		f(stored)
	}
	return stored, true, nil
}

// ByNonce is the message a client's nonce stored, so a client that lost the
// reply can tell whether to send again.
func (s *Service) ByNonce(ctx context.Context, thread, nonce string) (Message, bool, error) {
	if thread == "" {
		thread = Main
	}
	var m Message
	var found bool
	err := s.book.Read(ctx, func(tx *ledger.ReadTx) error {
		id, ok, err := durable.Get[string](tx, nonceKind, durable.Key(thread, nonce))
		if err != nil || !ok {
			return err
		}
		m, found, err = durable.Get[Message](tx, messageKind, id)
		return err
	})
	return m, found, err
}

// Recent is the last n messages of a thread, oldest first.
func (s *Service) Recent(ctx context.Context, thread string, n int) ([]Message, error) {
	if thread == "" {
		thread = Main
	}
	if n <= 0 || n > 500 {
		n = 100
	}
	var out []Message
	err := s.book.Read(ctx, func(tx *ledger.ReadTx) error {
		rows, err := tx.Query(`SELECT id,data FROM bindings WHERE kind=? AND json_extract(data,'$.thread')=? ORDER BY id DESC LIMIT ?`, messageKind, thread, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				return err
			}
			m, err := durable.Decode[Message](tx, raw, messageKind, id)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}
