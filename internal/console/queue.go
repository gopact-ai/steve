package console

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

type Exchange = consoleapi.Exchange

// Only Exchange is serialized. Request lifetimes and waiters belong to this
// process; accepted work belongs to the durable console document.
type queuedExchange struct {
	Exchange
	// Submission identity survives edits/steering. A keyed exchange's result
	// outlives the bounded transcript projection so restart retries can reply.
	PayloadHash          string              `json:"payload_hash,omitempty"`
	QuoteAliases         map[string]string   `json:"quote_aliases,omitempty"`
	Receipt              *consoleapi.Reply   `json:"receipt,omitempty"`
	RecoveryStopTarget   *recoveryStopTarget `json:"recovery_stop_target,omitempty"`
	RecoveryStop         *consoleapi.Reply   `json:"recovery_stop,omitempty"`
	RecoveryStopPending  string              `json:"recovery_stop_pending,omitempty"`
	ContinuationRejected bool                `json:"continuation_rejected,omitempty"`
	RecoveryPending      bool                `json:"recovery_pending,omitempty"`
	recoveryStopping     chan struct{}
	ctx                  context.Context
	cancel               context.CancelFunc
	done                 chan struct{}
	outcome              outcome
}

// ConversationID is a conversation's full identity: the console's own
// prefix and the name, whichever half the caller has.
func ConversationID(conversation string) string {
	if conversation == "" {
		return Prefix + "main"
	}
	if !strings.HasPrefix(conversation, Prefix) {
		return Prefix + conversation
	}
	return conversation
}

func interruptInput(input string) (prefix, rest string) {
	parsed := turn.ParseInput(input)
	if parsed.Interrupt {
		return parsed.Prefix, parsed.Prompt
	}
	return "", strings.TrimSpace(input)
}

func isInterrupt(input string) bool {
	prefix, _ := interruptInput(input)
	return prefix != ""
}

func (s *Service) parseInput(input string) (string, turn.ParsedInput) {
	return s.coordinator.ParseInput(input)
}

// stopControl is a line that only stops work: pressing stop sends one.
// The transcript stays what was said and answered, so the control and its
// receipt are kept but not drawn; the turn it stopped already says it was
// stopped.
func (s *Service) stopControl(input string) bool {
	_, parsed := s.parseInput(input)
	return parsed.Command == protocol.CommandCancel
}

func (s *Service) immediate(input string) bool {
	_, parsed := s.parseInput(input)
	return parsed.Immediate()
}

func copyExchange(e Exchange) Exchange {
	e.Quotes = append([]QuoteRef(nil), e.Quotes...)
	e.Refs = copyRefs(e.Refs)
	e.Materials = copyMaterials(e.Materials)
	return e
}

// Enqueue returns as soon as the submission is durable. The first line in
// an idle conversation starts here; no browser is needed to drain the rest.
func (s *Service) Enqueue(ctx context.Context, conversation, input string, quotes []QuoteRef) (Exchange, error) {
	return s.EnqueueCommand(ctx, conversation, input, "", quotes)
}

// EnqueueCommand uses the same durable exchange as SendCommand, returning
// immediately after acceptance or when replaying its current state.
func (s *Service) EnqueueCommand(ctx context.Context, conversation, input, commandID string, quotes []QuoteRef) (Exchange, error) {
	_, exchange, err := s.enqueue(ctx, conversation, input, quotes, enqueueOptions{Key: clientKey(commandID)})
	return exchange, err
}

// enqueue accepts one line. prompt, when set, is what the agent gets
// instead of the input; front puts the line ahead of everything still
// waiting, behind what already ran or runs.
type enqueueOptions struct {
	ResumeAdmission task.ResumeAdmission
	Prompt, Key     string
	Front           bool
	// Deferred accepts recovery input durably without starting it before
	// the caller has reconciled the original execution's accounting.
	Deferred bool
	// RewindTo names a line already sent that this one replaces: the
	// thread goes back to just before it and carries what was said
	// earlier into the prompt. See rewind.go.
	RewindTo                                         string
	Origin, Requester, ExpectedProject, ExpectedTask string
	Refs                                             []material.Ref
	Locale                                           string
}

func (s *Service) enqueue(ctx context.Context, conversation, input string, quotes []QuoteRef, options enqueueOptions) (*queuedExchange, Exchange, error) {
	prompt, key := options.Prompt, options.Key
	if s.owner == "" {
		return nil, Exchange{}, errors.New("the console needs feishu.owner_open_id: it acts as the owner")
	}
	conversation = ConversationID(conversation)
	input, prompt, quotes, hash := submission(input, prompt, quotes)
	s.mu.Lock()
	if s.closing || s.maintenance || s.recoveryStoppedLocked() {
		s.mu.Unlock()
		return nil, Exchange{}, consoleapi.ErrConsoleClosing
	}
	var previous *queuedExchange
	if key != "" {
		for _, item := range s.exchanges[conversation] {
			if item.Key == key {
				previous = item
				break
			}
		}
	}
	var aliases map[string]string
	if previous != nil {
		aliases = maps.Clone(previous.QuoteAliases)
	}
	if options.Locale == "" {
		options.Locale = s.submissionLocaleLocked(ctx, previous)
	}
	s.mu.Unlock()
	if len(aliases) > 0 {
		original := append([]QuoteRef(nil), quotes...)
		for i := range original {
			if id := aliases[original[i].Conversation]; id != "" {
				original[i].Conversation = id
			}
		}
		_, _, _, hash = submission(input, prompt, original)
	}
	if options.Locale != "" && options.Locale != "zh" && options.Locale != "en" {
		return nil, Exchange{}, errors.New("unsupported locale")
	}
	if len(options.Refs) > 0 || options.Locale != "" {
		hash = extendedSubmissionHash(hash, options.Refs, options.Locale)
	}
	// Only an explicitly supplied project changes client request identity.
	// Existing callers without it keep their established receipt hashes.
	if options.ExpectedProject != "" && strings.HasPrefix(key, "client:") {
		hash = projectSubmissionHash(hash, options.ExpectedProject)
	}
	s.mu.Lock()
	existing, err := s.submittedLocked(conversation, key, hash)
	s.mu.Unlock()
	if existing != nil || err != nil {
		if err != nil {
			return nil, Exchange{}, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.resumeLocked(existing)
	}
	if strings.TrimSpace(input) == "" && len(options.Refs) == 0 {
		return nil, Exchange{}, errors.New("input is required")
	}
	if _, err := s.quoteBlock(ctx, conversation, quotes); err != nil {
		return nil, Exchange{}, err
	}
	frozen, project, err := s.freezeMaterials(ctx, conversation, options.Refs)
	if err != nil {
		return nil, Exchange{}, err
	}
	if project, err = s.confirmExpectedProject(ctx, conversation, key, options.ExpectedProject, project); err != nil {
		return nil, Exchange{}, err
	}
	if len(options.Refs) > 0 {
		if _, parsed := s.parseInput(input); parsed.Control() {
			return nil, Exchange{}, errors.New("materials cannot be attached to a control command")
		}
	}
	if options.ExpectedProject == "" {
		options.ExpectedProject = project
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.maintenance || s.recoveryStoppedLocked() {
		return nil, Exchange{}, consoleapi.ErrConsoleClosing
	}
	if other, err := s.submittedLocked(conversation, key, hash); other != nil || err != nil {
		if err != nil {
			return nil, Exchange{}, err
		}
		return s.resumeLocked(other)
	}
	e := &queuedExchange{
		Exchange: Exchange{ID: "e" + strings.TrimPrefix(newReplyID(), "r"), Conversation: conversation, Input: input, Prompt: prompt, Key: key,
			Origin: options.Origin, Requester: options.Requester, ExpectedProject: options.ExpectedProject, ExpectedTask: options.ExpectedTask,
			ResumeAdmission: options.ResumeAdmission,
			Refs:            copyRefs(options.Refs), Materials: frozen, Locale: options.Locale,
			Quotes: append([]QuoteRef(nil), quotes...), State: consoleapi.ExchangeQueued, EnqueuedAt: time.Now().UTC()},
		PayloadHash: hash, ctx: s.exchangeContext(ctx), done: make(chan struct{}),
		RecoveryPending: options.Deferred && options.ResumeAdmission == (task.ResumeAdmission{}),
	}
	if !strings.HasPrefix(key, "client:") {
		e.PayloadHash = ""
	}
	if options.RewindTo != "" {
		return s.acceptRewoundLocked(e, options.RewindTo, options.Front)
	}
	return s.acceptExchange(e, options.Front, options.Deferred)
}

// localeTextLocked is the catalog a request is answered in: its own
// locale, else the request context's, else the console's default.
func (s *Service) localeTextLocked(ctx context.Context, locale string) i18n.Catalog {
	if locale == "" {
		locale = s.submissionLocaleLocked(ctx, nil)
	}
	return i18n.New(i18n.FromLang(locale))
}

// submissionLocaleLocked is the locale a line without one is answered in:
// the one its earlier submission had, else the request's, else the
// console's default.
func (s *Service) submissionLocaleLocked(ctx context.Context, previous *queuedExchange) string {
	if previous != nil {
		return previous.Locale
	}
	if locale := string(i18n.ContextLocale(ctx)); locale != "" {
		return locale
	}
	if s.DefaultLocaleSource != nil {
		return s.DefaultLocaleSource()
	}
	return s.defaultLocale
}

// confirmExpectedProject returns the project the line runs under. A client
// that named one is refused when the conversation is bound elsewhere; the
// binding is read only when the materials did not already settle it.
func (s *Service) confirmExpectedProject(ctx context.Context, conversation, key, expected, project string) (string, error) {
	if expected == "" {
		return project, nil
	}
	if project == "" {
		state, err := s.Context(ctx, conversation)
		if err != nil {
			return "", err
		}
		if state.Project != nil {
			project = state.Project.ID
		}
	}
	if strings.HasPrefix(key, "client:") && expected != project {
		return "", fmt.Errorf("expected project %s, but conversation is bound to %s", expected, project)
	}
	return project, nil
}

// resumeLocked replays a submission this conversation has already
// accepted: the exchange it made, and a retry of the stop a recovery is
// still waiting on.
func (s *Service) resumeLocked(existing *queuedExchange) (*queuedExchange, Exchange, error) {
	existing, err := s.retryRecoveryStopLocked(existing)
	if err != nil {
		return nil, Exchange{}, err
	}
	return existing, copyExchange(existing.Exchange), nil
}

func (s *Service) acceptExchangeLocked(e *queuedExchange, front bool) (*queuedExchange, Exchange, error) {
	return s.acceptExchange(e, front, false)
}

func (s *Service) acceptExchange(e *queuedExchange, front, deferred bool) (*queuedExchange, Exchange, error) {
	conversation := e.Conversation
	if s.sealed[conversation] {
		return nil, Exchange{}, fmt.Errorf("%w: %s is being deleted", consoleapi.ErrBusy, conversation)
	}
	var err error
	list := s.exchanges[conversation]
	at := len(list)
	if front {
		for i, other := range list {
			if other.State == consoleapi.ExchangeQueued {
				at = i
				break
			}
		}
	}
	list = append(list, nil)
	copy(list[at+1:], list[at:])
	list[at] = e
	s.exchanges[conversation] = list
	if deferred {
		err = s.save()
		if err == nil {
			s.publishQueue(conversation)
		}
	} else if s.immediate(e.Input) {
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
	list := s.exchanges[ConversationID(conversation)]
	remaining := keep
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		if e.State.Terminal() {
			if remaining == 0 {
				continue
			}
			remaining--
		}
		// The carried history is for the agent, not the page: it can run
		// to a hundred kilobytes and nothing in the queue view reads it.
		listed := copyExchange(e.Exchange)
		listed.History = ""
		out = append(out, listed)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (s *Service) queuedLocked(id string) (*queuedExchange, int, error) {
	for _, list := range s.exchanges {
		for i, e := range list {
			if e.ID == id {
				if e.State != consoleapi.ExchangeQueued {
					return nil, 0, consoleapi.ErrExchangeNotQueued
				}
				return e, i, nil
			}
		}
	}
	return nil, 0, consoleapi.ErrExchangeNotFound
}

func (s *Service) DeleteQueued(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, index, err := s.queuedLocked(id)
	if err != nil {
		return err
	}
	list := s.exchanges[e.Conversation]
	if e.Key != "" {
		previousState, previousReceipt := e.State, e.Receipt
		e.State = consoleapi.ExchangeCancelled
		r := consoleapi.Reply{Conversation: e.Conversation, ExchangeID: e.ID, Kind: "reply", Text: "queued exchange cancelled", Error: "queued exchange cancelled"}
		e.Receipt = &r
		if err := s.save(); err != nil {
			e.State, e.Receipt = previousState, previousReceipt
			return err
		}
		e.outcome = replyOutcome(r)
		close(e.done)
		s.publishQueue(e.Conversation)
		return nil
	}
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
	if s.closing || s.maintenance || s.recoveryStoppedLocked() {
		return consoleapi.ErrConsoleClosing
	}
	if e.RecoveryPending {
		return fmt.Errorf("%w: original task accounting is pending", consoleapi.ErrBusy)
	}
	if e.ResumeAdmission != (task.ResumeAdmission{}) {
		if err := s.checkResumeLocked(e); err != nil {
			return err
		}
	}
	s.bindRecoveryStopTargetLocked(e)
	conversation := e.Conversation
	previous := s.replies[conversation]
	e.State, e.StartedAt = consoleapi.ExchangeRunning, time.Now().UTC()
	s.running[conversation]++
	sent := s.recordLocked(consoleapi.Reply{At: e.StartedAt, Conversation: conversation, ProjectID: e.ExpectedProject, ExchangeID: e.ID, Input: e.Input, Kind: "sent", Silent: s.stopControl(e.Input), Relayed: relayed(e.Exchange), Refs: copyRefs(e.Refs), Materials: copyMaterials(e.Materials)})
	if err := s.save(); err != nil {
		e.State, e.StartedAt = consoleapi.ExchangeQueued, time.Time{}
		s.running[conversation]--
		s.replies[conversation] = previous
		return err
	}
	if isInterrupt(e.Input) {
		// Also cancel a reserved turn that has not entered Handle yet. The
		// prefix still makes the coordinator interrupt any active session.
		for _, active := range s.exchanges[conversation] {
			if active != e && active.State == consoleapi.ExchangeRunning && active.cancel != nil {
				active.cancel()
			}
		}
	}
	ctx, cancel := context.WithCancel(e.ctx)
	e.cancel = cancel
	s.publishReply(sent)
	s.publishQueue(conversation)
	exchange := copyExchange(e.Exchange)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		reply, err := s.runExchange(ctx, exchange)
		if s.continueDetached(e, err) {
			return
		}
		s.finish(e, reply, err)
	}()
	return nil
}

func (s *Service) startNextLocked(conversation string) error {
	if s.closing || s.maintenance || s.recoveryStoppedLocked() {
		return nil
	}
	if s.running[conversation] != 0 {
		return nil
	}
	for _, e := range s.exchanges[conversation] {
		if e.RecoveryPending || e.State == consoleapi.ExchangeRecovering || e.State == consoleapi.ExchangeAwaitingUser {
			err := s.save()
			if err == nil {
				s.publishQueue(conversation)
			}
			return err
		}
	}
	for _, e := range s.exchanges[conversation] {
		if e.State == consoleapi.ExchangeQueued {
			if e.ResumeAdmission != (task.ResumeAdmission{}) {
				if err := s.checkResumeLocked(e); err != nil {
					if errors.Is(err, task.ErrResumePending) {
						continue
					}
					if errors.Is(err, task.ErrExecutionStopped) || errors.Is(err, task.ErrResumeConsumed) {
						if err := s.rejectResumeLocked(e, err); err != nil {
							return err
						}
						continue
					}
					return err
				}
			}
			return s.startLocked(e)
		}
	}
	return nil
}

func (s *Service) finish(e *queuedExchange, reply consoleapi.Reply, err error) {
	s.mu.Lock()
	for e.recoveryStopping != nil {
		done := e.recoveryStopping
		s.mu.Unlock()
		<-done
		s.mu.Lock()
	}
	if e.State.Terminal() {
		s.mu.Unlock()
		return
	}
	if s.recoveryStoppedLocked() {
		s.detachRecoveryLocked(e, context.Canceled)
		s.mu.Unlock()
		return
	}
	if e.RecoveryStop == nil && e.RecoveryStopPending != "" {
		s.mu.Unlock()
		s.waitRecoveryStop(e)
		return
	}
	if e.RecoveryStop != nil {
		reply, err = *e.RecoveryStop, nil
	}
	var interrupted []consoleapi.PendingQuestion
	for id, q := range s.questions {
		if q.ExchangeID == e.ID && q.State == "pending" {
			q.State = "interrupted"
			q.UpdatedAt = time.Now().UTC()
			s.questions[id] = q
			interrupted = append(interrupted, q)
		}
	}
	if work := s.processes[e.ID]; work != nil {
		reply.Process = work.summary()
		delete(s.processes, e.ID)
	}
	reply.At, reply.Kind, reply.Conversation, reply.ExchangeID = time.Now().UTC(), "reply", e.Conversation, e.ID
	reply.Silent = s.stopControl(e.Input)
	if reply.ProjectID == "" {
		reply.ProjectID = e.ExpectedProject
	}
	if err != nil {
		reply.Error = err.Error()
		if reply.Text == "" {
			reply.Text = reply.Error
		}
	}
	reply = s.recordLocked(reply)
	e.ReplyID, e.State = reply.ID, consoleapi.ExchangeDone
	if e.RecoveryStop != nil {
		e.State = consoleapi.ExchangeCancelled
	}
	if err != nil {
		e.State = consoleapi.ExchangeFailed
	}
	e.ContinuationRejected = e.ExpectedTask != "" && errors.Is(err, task.ErrContinuationUnavailable)
	e.outcome = outcome{reply: reply, err: err}
	if e.Key != "" || reply.AttemptID != "" {
		e.Receipt = &reply
	}
	s.trimExchangesLocked(e.Conversation)
	// Keep the reservation until the terminal state is durable. Retrying a
	// document write must never invoke the coordinator a second time.
	for s.save() != nil {
		if s.recoveryStoppedLocked() {
			s.detachRecoveryLocked(e, context.Canceled)
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		time.Sleep(time.Second)
		s.mu.Lock()
	}
	s.running[e.Conversation]--
	for _, q := range interrupted {
		if waiter := s.questionWaiters[q.ID]; waiter != nil {
			close(waiter)
			delete(s.questionWaiters, q.ID)
		}
		s.publishQuestion(q)
	}
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

// Pending work and keyed business records are never evicted. Only unkeyed
// terminal history is bounded; Queue separately limits its UI projection.
func (s *Service) trimExchangesLocked(conversation string) {
	list := s.exchanges[conversation]
	remaining := keep
	out := make([]*queuedExchange, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		if e.State.Terminal() && e.Key == "" && (e.Receipt == nil || e.Receipt.AttemptID == "") {
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
			e.ctx, e.done = s.exchangeContext(context.Background()), make(chan struct{})
			_, parsed := s.parseInput(e.Input)
			if s.recoveryLifetime != nil && !parsed.Control() && (e.State == consoleapi.ExchangeRunning || e.State == consoleapi.ExchangeRecovering || e.State == consoleapi.ExchangeAwaitingUser) {
				e.State = consoleapi.ExchangeRecovering
				s.running[conversation]++
				continue
			}
			if e.State == consoleapi.ExchangeRunning {
				err := errors.New("console restarted before this exchange completed")
				r := s.recordLocked(consoleapi.Reply{At: time.Now().UTC(), Conversation: conversation, ExchangeID: e.ID, Kind: "reply", Text: err.Error(), Error: err.Error()})
				e.State, e.ReplyID = consoleapi.ExchangeFailed, r.ID
				if e.Key != "" {
					e.Receipt = &r
				}
			}
			if e.State.Terminal() {
				if e.Receipt != nil {
					e.outcome = replyOutcome(*e.Receipt)
				} else {
					for _, r := range s.replies[conversation] {
						if r.ID == e.ReplyID {
							e.outcome = replyOutcome(r)
							break
						}
					}
				}
				e.ctx = nil
				close(e.done)
			}
		}
		s.trimExchangesLocked(conversation)
	}
	s.markRelayedLocked()
	return s.save()
}

// Drain starts every conversation's next waiting exchange. It is its own
// step so that what a restart must continue can be put in line first.
func (s *Service) Drain() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []*queuedExchange
	for _, list := range s.exchanges {
		for _, e := range list {
			if e.RecoveryPending {
				pending = append(pending, e)
				e.RecoveryPending = false
			}
		}
	}
	if len(pending) > 0 {
		if err := s.save(); err != nil {
			for _, e := range pending {
				e.RecoveryPending = true
			}
			return err
		}
	}
	for conversation := range s.exchanges {
		if err := s.startNextLocked(conversation); err != nil {
			return err
		}
	}
	return nil
}
