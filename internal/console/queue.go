package console

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
)

type Exchange = readmodel.Exchange

// Only Exchange is serialized. Request lifetimes and waiters belong to this
// process; accepted work belongs to the durable console document.
type queuedExchange struct {
	Exchange
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	outcome outcome
}

func conversationID(conversation string) string {
	if conversation == "" {
		return Prefix + "main"
	}
	if !strings.HasPrefix(conversation, Prefix) {
		return Prefix + conversation
	}
	return conversation
}

func interruptInput(input string) (prefix, rest string) {
	input = strings.TrimSpace(input)
	for _, prefix := range []string{"!", "！"} {
		if rest, ok := strings.CutPrefix(input, prefix); ok && strings.TrimSpace(rest) != "" {
			return prefix, strings.TrimSpace(rest)
		}
	}
	return "", input
}

func isInterrupt(input string) bool {
	prefix, _ := interruptInput(input)
	return prefix != ""
}

func immediate(input string) bool {
	// A stop must reach the coordinator while its target is still running.
	fields := strings.Fields(input)
	return isInterrupt(input) || (len(fields) > 0 && fields[0] == "/cancel")
}

func copyExchange(e Exchange) Exchange {
	e.Quotes = append([]QuoteRef(nil), e.Quotes...)
	return e
}

// Enqueue returns as soon as the submission is durable. The first line in
// an idle conversation starts here; no browser is needed to drain the rest.
func (s *Service) Enqueue(ctx context.Context, conversation, input string, quotes []QuoteRef) (Exchange, error) {
	_, exchange, err := s.enqueue(ctx, conversation, input, "", quotes, false)
	return exchange, err
}

// enqueue accepts one line. prompt, when set, is what the agent gets
// instead of the input; front puts the line ahead of everything still
// waiting, behind what already ran or runs.
func (s *Service) enqueue(ctx context.Context, conversation, input, prompt string, quotes []QuoteRef, front bool) (*queuedExchange, Exchange, error) {
	if s.owner == "" {
		return nil, Exchange{}, errors.New("the console needs feishu.owner_open_id: it acts as the owner")
	}
	if strings.TrimSpace(input) == "" {
		return nil, Exchange{}, errors.New("input is required")
	}
	conversation = conversationID(conversation)
	if _, err := s.quoteBlock(ctx, conversation, quotes); err != nil {
		return nil, Exchange{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &queuedExchange{
		Exchange: Exchange{ID: "e" + strings.TrimPrefix(newReplyID(), "r"), Conversation: conversation, Input: input, Prompt: prompt,
			Quotes: append([]QuoteRef(nil), quotes...), State: "queued", EnqueuedAt: time.Now().UTC()},
		ctx: context.WithoutCancel(ctx), done: make(chan struct{}),
	}
	list := s.exchanges[conversation]
	at := len(list)
	if front {
		for i, other := range list {
			if other.State == "queued" {
				at = i
				break
			}
		}
	}
	list = append(list, nil)
	copy(list[at+1:], list[at:])
	list[at] = e
	s.exchanges[conversation] = list
	var err error
	if immediate(input) {
		err = s.startLocked(e)
	} else if s.running[conversation] == 0 {
		err = s.startNextLocked(conversation)
	} else {
		err = s.save()
		if err == nil {
			s.publishQueue(conversation)
		}
	}
	if err != nil {
		s.exchanges[conversation] = append(list[:at:at], list[at+1:]...)
		return nil, Exchange{}, err
	}
	return e, copyExchange(e.Exchange), nil
}

// Queue includes recent terminal exchanges so HTTP callers can correlate
// completion with replies. The composer projects only queued entries.
func (s *Service) Queue(conversation string) []Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Exchange{}
	for _, e := range s.exchanges[conversationID(conversation)] {
		out = append(out, copyExchange(e.Exchange))
	}
	return out
}

func (s *Service) queuedLocked(id string) (*queuedExchange, int, error) {
	for _, list := range s.exchanges {
		for i, e := range list {
			if e.ID == id {
				if e.State != "queued" {
					return nil, 0, readmodel.ErrExchangeNotQueued
				}
				return e, i, nil
			}
		}
	}
	return nil, 0, readmodel.ErrExchangeNotFound
}

func (s *Service) DeleteQueued(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, index, err := s.queuedLocked(id)
	if err != nil {
		return err
	}
	list := s.exchanges[e.Conversation]
	next := append([]*queuedExchange(nil), list[:index]...)
	s.exchanges[e.Conversation] = append(next, list[index+1:]...)
	if err := s.save(); err != nil {
		s.exchanges[e.Conversation] = list
		return err
	}
	e.outcome.err = errors.New("queued exchange deleted")
	close(e.done)
	s.publishQueue(e.Conversation)
	return nil
}

func (s *Service) EditQueued(id, input string) (Exchange, error) {
	if strings.TrimSpace(input) == "" {
		return Exchange{}, errors.New("input is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, _, err := s.queuedLocked(id)
	if err != nil {
		return Exchange{}, err
	}
	previous := e.Input
	e.Input = input
	if err := s.save(); err != nil {
		e.Input = previous
		return Exchange{}, err
	}
	s.publishQueue(e.Conversation)
	return copyExchange(e.Exchange), nil
}

func (s *Service) Steer(_ context.Context, id string) (Exchange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, _, err := s.queuedLocked(id)
	if err != nil {
		return Exchange{}, err
	}
	previous := e.Input
	if !isInterrupt(e.Input) {
		e.Input = "!" + e.Input
	}
	if err := s.startLocked(e); err != nil {
		e.Input = previous
		return Exchange{}, err
	}
	return copyExchange(e.Exchange), nil
}

// startLocked reserves the conversation before launching a goroutine. Even
// concurrent submissions cannot both observe an idle queue and start it.
func (s *Service) startLocked(e *queuedExchange) error {
	conversation := e.Conversation
	previous := s.replies[conversation]
	e.State, e.StartedAt = "running", time.Now().UTC()
	s.running[conversation]++
	sent := s.recordLocked(readmodel.Reply{At: e.StartedAt, Conversation: conversation, ExchangeID: e.ID, Input: e.Input, Kind: "sent"})
	if err := s.save(); err != nil {
		e.State, e.StartedAt = "queued", time.Time{}
		s.running[conversation]--
		s.replies[conversation] = previous
		return err
	}
	if isInterrupt(e.Input) {
		// Also cancel a reserved turn that has not entered Handle yet. The
		// prefix still makes the coordinator interrupt any active session.
		for _, active := range s.exchanges[conversation] {
			if active != e && active.State == "running" && active.cancel != nil {
				active.cancel()
			}
		}
	}
	ctx, cancel := context.WithCancel(e.ctx)
	e.cancel = cancel
	s.publishReply(sent)
	s.publishQueue(conversation)
	exchange := copyExchange(e.Exchange)
	go func() {
		defer cancel()
		reply, err := s.runExchange(ctx, exchange)
		s.finish(e, reply, err)
	}()
	return nil
}

func (s *Service) startNextLocked(conversation string) error {
	if s.running[conversation] != 0 {
		return nil
	}
	for _, e := range s.exchanges[conversation] {
		if e.State == "queued" {
			return s.startLocked(e)
		}
	}
	return nil
}

func (s *Service) finish(e *queuedExchange, reply readmodel.Reply, err error) {
	s.mu.Lock()
	reply.At, reply.Kind, reply.Conversation, reply.ExchangeID = time.Now().UTC(), "reply", e.Conversation, e.ID
	if err != nil {
		reply.Error = err.Error()
		if reply.Text == "" {
			reply.Text = reply.Error
		}
	}
	reply = s.recordLocked(reply)
	e.ReplyID, e.State = reply.ID, "done"
	if err != nil {
		e.State = "failed"
	}
	e.outcome = outcome{reply: reply, err: err}
	s.trimExchangesLocked(e.Conversation)
	// Keep the reservation until the terminal state is durable. Retrying a
	// document write must never invoke the handler a second time.
	for s.save() != nil {
		s.mu.Unlock()
		time.Sleep(time.Second)
		s.mu.Lock()
	}
	s.running[e.Conversation]--
	s.publishReply(reply)
	s.publishQueue(e.Conversation)
	e.ctx, e.cancel = nil, nil
	close(e.done)
	for s.startNextLocked(e.Conversation) != nil {
		s.mu.Unlock()
		time.Sleep(time.Second)
		s.mu.Lock()
	}
	s.mu.Unlock()
}

// Pending work is never evicted. Terminal history has the same bound as
// replies so long-lived conversations do not grow the document forever.
func (s *Service) trimExchangesLocked(conversation string) {
	list := s.exchanges[conversation]
	remaining := keep
	out := make([]*queuedExchange, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		if e.State == "done" || e.State == "failed" {
			if remaining == 0 {
				continue
			}
			remaining--
		}
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	s.exchanges[conversation] = out
}

func (s *Service) publishQueue(conversation string) {
	if s.model != nil {
		s.model.Publish(readmodel.Event{Kind: "console.queue", Conversation: conversation})
	}
}

// A running turn may already have changed external state before a crash.
// Record the interruption rather than replaying it; untouched queued work
// can safely resume, in order, after the complete document is loaded.
func (s *Service) restoreQueueLocked() error {
	for conversation, list := range s.exchanges {
		for _, e := range list {
			e.ctx, e.done = context.Background(), make(chan struct{})
			if e.State == "running" {
				err := errors.New("console restarted before this exchange completed")
				r := s.recordLocked(readmodel.Reply{At: time.Now().UTC(), Conversation: conversation, ExchangeID: e.ID, Kind: "reply", Text: err.Error(), Error: err.Error()})
				e.State, e.ReplyID = "failed", r.ID
			}
			if e.State == "done" || e.State == "failed" {
				e.ctx = nil
				close(e.done)
			}
		}
		s.trimExchangesLocked(conversation)
	}
	return s.save()
}

// Drain starts every conversation's next waiting exchange. It is its own
// step so that what a restart must continue can be put in line first.
func (s *Service) Drain() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conversation := range s.exchanges {
		if err := s.startNextLocked(conversation); err != nil {
			return err
		}
	}
	return nil
}
