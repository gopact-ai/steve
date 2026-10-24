// Package approvals turns what an executor asks of the person into cards:
// a permission it wants before a step, or a question only the person can
// answer. In auto mode the executor's model decides when to ask and each ask
// becomes a card; a card nobody answers in time is refused. In yolo mode a
// permission is granted at once and recorded, and questions still reach the
// person, since nothing can answer them in the person's place.
package approvals

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/feed"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

const (
	KindPermission = "permission"
	KindQuestion   = "question"

	StateOpen     = "open"
	StateApproved = "approved"
	StateRejected = "rejected"
	StateAnswered = "answered"
	StateExpired  = "expired"

	EventOpened   = "approval.opened"
	EventResolved = "approval.resolved"

	ModeYolo = "yolo"
	// ModeDeny refuses at once, with a record: for executions that only
	// propose (the chief of staff) and ask the person in their reply instead.
	ModeDeny = "deny"
)

// Option is one answer a card offers.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Allow bool   `json:"allow,omitempty"`
}

// Card is one thing waiting on the person.
type Card struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"task_id"`
	Kind          string    `json:"kind"`
	Title         string    `json:"title"`
	Detail        string    `json:"detail,omitempty"`
	Options       []Option  `json:"options,omitempty"`
	AllowFreeText bool      `json:"allow_free_text,omitempty"`
	State         string    `json:"state"`
	Choice        string    `json:"choice,omitempty"`
	Text          string    `json:"text,omitempty"`
	Auto          bool      `json:"auto,omitempty"` // granted by yolo, not by a person
	OpenedAt      time.Time `json:"opened_at"`
	Deadline      time.Time `json:"deadline"`
	ResolvedAt    time.Time `json:"resolved_at,omitzero"`
}

// Decision is the person's answer to a card.
type Decision struct {
	Approve bool   `json:"approve,omitempty"`
	Choice  string `json:"choice,omitempty"`
	Text    string `json:"text,omitempty"`
}

// Tasks is what approvals needs from the task service.
type Tasks interface {
	ModeOf(taskID string) string
	Waiting(taskID string) (done func())
}

var (
	ErrNotFound = errors.New("approvals: card not found")
	ErrClosed   = errors.New("approvals: card is no longer open")
	ErrInvalid  = errors.New("approvals: invalid decision")
)

type Service struct {
	tasks   Tasks
	feed    *feed.Feed
	timeout time.Duration
	now     func() time.Time

	mu     sync.Mutex
	nextID int
	cards  map[string]*pending
	order  []string
}

type pending struct {
	card    Card
	answers chan Decision
}

// keep bounds how many resolved cards stay readable.
const keep = 200

func New(tasks Tasks, events *feed.Feed, timeout time.Duration) *Service {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &Service{tasks: tasks, feed: events, timeout: timeout, now: time.Now, cards: map[string]*pending{}}
}

// Ask is the executor's permission request: a permission.AskFunc.
func (s *Service) Ask(ctx context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
	taskID := taskOf(ctx)
	options := make([]Option, 0, len(ask.Options))
	for _, o := range ask.Options {
		allow := o.Kind == acp.PermissionOptionKindAllowOnce || o.Kind == acp.PermissionOptionKindAllowAlways
		options = append(options, Option{ID: string(o.OptionID), Label: o.Name, Allow: allow})
	}
	title := strings.TrimSpace(ask.ToolName)
	if title == "" {
		title = "Permission for " + string(ask.Kind)
	}
	card := Card{TaskID: taskID, Kind: KindPermission, Title: title, Detail: ask.Reason, Options: options}
	switch s.modeOf(taskID) {
	case ModeYolo:
		card.Auto = true
		s.record(card, StateApproved, Decision{Approve: true})
		return permission.Choose(true, ask.Options), nil
	case ModeDeny:
		card.Auto = true
		s.record(card, StateRejected, Decision{})
		return permission.Choose(false, ask.Options), nil
	}
	decision, state, err := s.wait(ctx, card)
	if err != nil || state != StateApproved {
		return permission.Choose(false, ask.Options), err
	}
	if decision.Choice != "" {
		for _, o := range ask.Options {
			if string(o.OptionID) == decision.Choice {
				return acp.SelectedRequestPermissionOutcome(o.OptionID), nil
			}
		}
	}
	return permission.Choose(true, ask.Options), nil
}

// AskUser is the executor's question: an acphost.AskUserFunc.
func (s *Service) AskUser(ctx context.Context, q view.Question) (view.Answer, error) {
	options := make([]Option, 0, len(q.Choices))
	for _, c := range q.Choices {
		options = append(options, Option{ID: c.Value, Label: c.Label})
	}
	title := strings.TrimSpace(q.Title)
	if title == "" {
		title = strings.TrimSpace(q.Message)
	}
	card := Card{TaskID: taskOf(ctx), Kind: KindQuestion, Title: title, Detail: q.Message, Options: options, AllowFreeText: q.AllowFreeText || len(q.Choices) == 0}
	if s.modeOf(card.TaskID) == ModeDeny {
		card.Auto = true
		s.record(card, StateRejected, Decision{})
		return view.Answer{Decision: "decline"}, nil
	}
	decision, state, err := s.wait(ctx, card)
	if err != nil {
		return view.Answer{}, err
	}
	if state != StateAnswered {
		return view.Answer{Decision: "decline"}, nil
	}
	return view.Answer{Value: decision.Choice, Text: decision.Text, Decision: "accept"}, nil
}

// wait opens the card and blocks until it is decided, expires, or the
// execution ends. An execution that ends first leaves the card expired.
func (s *Service) wait(ctx context.Context, card Card) (Decision, string, error) {
	// The task shows as waiting before the card can be seen, so nobody
	// reads an open card on a task that still says running.
	if card.TaskID != "" && s.tasks != nil {
		defer s.tasks.Waiting(card.TaskID)()
	}
	p := s.open(card)
	s.mu.Lock()
	deadline, id := p.card.Deadline, p.card.ID
	s.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case d := <-p.answers:
		s.mu.Lock()
		state := p.card.State
		s.mu.Unlock()
		return d, state, nil
	case <-timer.C:
		s.expire(id)
		return Decision{}, StateExpired, nil
	case <-ctx.Done():
		s.expire(id)
		return Decision{}, StateExpired, ctx.Err()
	}
}

func (s *Service) open(card Card) *pending {
	p, opened := s.store(card)
	s.publish(EventOpened, opened)
	return p
}

// store keeps a new card without telling anyone, and returns it as stored:
// a copy taken under the lock, since an answer may already be changing it.
func (s *Service) store(card Card) (*pending, Card) {
	s.mu.Lock()
	s.nextID++
	card.ID = strconv.Itoa(s.nextID)
	card.State = StateOpen
	card.OpenedAt = s.now().UTC()
	card.Deadline = card.OpenedAt.Add(s.timeout)
	p := &pending{card: card, answers: make(chan Decision, 1)}
	s.cards[card.ID] = p
	s.order = append(s.order, card.ID)
	s.trimLocked()
	stored := p.card
	s.mu.Unlock()
	return p, stored
}

// record keeps a card decided without the person (yolo, deny). Nothing is
// put in front of them: only the resolution is published, for the record.
func (s *Service) record(card Card, state string, d Decision) {
	p, _ := s.store(card)
	s.mu.Lock()
	p.card.State, p.card.ResolvedAt = state, s.now().UTC()
	p.card.Choice, p.card.Text = d.Choice, d.Text
	resolved := p.card
	s.mu.Unlock()
	s.publish(EventResolved, resolved)
}

// Resolve answers an open card. A permission takes Approve and, optionally,
// the option chosen; a question takes a choice or free text.
func (s *Service) Resolve(id string, d Decision) (Card, error) {
	s.mu.Lock()
	p, ok := s.cards[id]
	if !ok {
		s.mu.Unlock()
		return Card{}, ErrNotFound
	}
	if p.card.State != StateOpen {
		card := p.card
		s.mu.Unlock()
		return card, ErrClosed
	}
	switch p.card.Kind {
	case KindPermission:
		if d.Choice != "" && !hasOption(p.card.Options, d.Choice) {
			s.mu.Unlock()
			return Card{}, fmt.Errorf("%w: unknown option %q", ErrInvalid, d.Choice)
		}
		p.card.State = StateRejected
		if d.Approve {
			p.card.State = StateApproved
		}
	case KindQuestion:
		d.Text = strings.TrimSpace(d.Text)
		switch {
		case d.Choice != "" && hasOption(p.card.Options, d.Choice):
		case d.Choice == "" && d.Text != "" && p.card.AllowFreeText:
		default:
			s.mu.Unlock()
			return Card{}, fmt.Errorf("%w: answer with one of the choices or text", ErrInvalid)
		}
		p.card.State = StateAnswered
	}
	p.card.Choice, p.card.Text, p.card.ResolvedAt = d.Choice, d.Text, s.now().UTC()
	card := p.card
	p.answers <- d
	s.mu.Unlock()
	s.publish(EventResolved, card)
	return card, nil
}

func (s *Service) expire(id string) {
	s.mu.Lock()
	p, ok := s.cards[id]
	if !ok || p.card.State != StateOpen {
		s.mu.Unlock()
		return
	}
	p.card.State, p.card.ResolvedAt = StateExpired, s.now().UTC()
	card := p.card
	s.mu.Unlock()
	s.publish(EventResolved, card)
}

// Open is every card still waiting on the person, oldest first.
func (s *Service) Open() []Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Card
	for _, id := range s.order {
		if p := s.cards[id]; p.card.State == StateOpen {
			out = append(out, p.card)
		}
	}
	return out
}

// Get is one card, open or resolved.
func (s *Service) Get(id string) (Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.cards[id]
	if !ok {
		return Card{}, ErrNotFound
	}
	return p.card, nil
}

func (s *Service) trimLocked() {
	for len(s.order) > keep {
		oldest := s.order[0]
		if s.cards[oldest].card.State == StateOpen {
			return
		}
		delete(s.cards, oldest)
		s.order = s.order[1:]
	}
}

func (s *Service) publish(kind string, card Card) {
	if s.feed != nil {
		_, _ = s.feed.Publish(kind, card)
	}
}

func (s *Service) modeOf(taskID string) string {
	if taskID == "" || s.tasks == nil {
		return ""
	}
	return s.tasks.ModeOf(taskID)
}

func hasOption(options []Option, id string) bool {
	for _, o := range options {
		if o.ID == id {
			return true
		}
	}
	return false
}

func taskOf(ctx context.Context) string {
	if key, ok := execution.KeyOf(ctx); ok {
		return key.TaskID
	}
	return ""
}
