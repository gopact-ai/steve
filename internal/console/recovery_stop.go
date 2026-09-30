package console

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type retainedStopDriver interface {
	StopRetainedTask(context.Context, string, turn.Request, bool) (turn.Result, error)
}

// stopRequested is what an exchange waits on once a stop of its original
// task has been asked for and nothing has confirmed it yet.
const stopRequested = "Stop requested; waiting for confirmation from the original task and its children."

// A recovery question is owned by the console after the coordinator's turn
// has detached. Stop must reach that original task, not an empty turn slot.
func (s *Service) stopRecovering(ctx context.Context, control Exchange, requester string) (turn.Result, bool, error) {
	s.mu.Lock()
	var controlRecord *queuedExchange
	for _, e := range s.exchanges[control.Conversation] {
		if e.ID == control.ID {
			controlRecord = e
			break
		}
	}
	var bound *recoveryStopTarget
	if controlRecord != nil {
		bound = controlRecord.RecoveryStopTarget
	}
	var target *queuedExchange
	for _, e := range s.exchanges[control.Conversation] {
		if e.ID == control.ID || (bound != nil && e.ID != bound.ExchangeID) || (bound == nil && !s.recoveryStopCandidateLocked(control.Conversation, control.ID, e)) {
			continue
		}
		if target != nil {
			s.mu.Unlock()
			return turn.Result{}, true, errors.New("multiple recovering tasks require an explicit task selection")
		}
		target = e
	}
	driver := s.recoveryDriver
	if target == nil {
		s.mu.Unlock()
		if bound != nil {
			return turn.Result{}, true, errors.New("original stop target is unavailable")
		}
		return turn.Result{}, false, nil
	}
	if bound != nil && (bound.Requester != requester || bound.Conversation != control.Conversation) {
		s.mu.Unlock()
		return turn.Result{}, true, errors.New("original stop target identity changed")
	}
	if target.RecoveryStop != nil {
		reply := *target.RecoveryStop
		s.mu.Unlock()
		return turn.Result{Text: reply.Text, Title: reply.Title}, true, nil
	}
	if target.State.Terminal() {
		s.mu.Unlock()
		return turn.Result{}, true, errors.New("original stop target has already finished without a stop receipt")
	}
	if target.Requester != "" && target.Requester != requester {
		s.mu.Unlock()
		return turn.Result{}, true, errors.New("recovery requires the original requester")
	}
	if controlRecord != nil && controlRecord.RecoveryStopTarget == nil {
		controlRecord.RecoveryStopTarget = &recoveryStopTarget{Conversation: control.Conversation, ExchangeID: target.ID, Requester: requester}
		if err := s.save(); err != nil {
			controlRecord.RecoveryStopTarget = nil
			s.mu.Unlock()
			return turn.Result{}, true, err
		}
	}
	if target.recoveryStopping != nil {
		s.mu.Unlock()
		return turn.Result{}, true, errors.New("the original recovery task is already stopping")
	}
	release := s.beginRecoveryStopLocked(target)
	defer release()
	original := copyExchange(target.Exchange)
	s.mu.Unlock()
	if driver == nil {
		return turn.Result{}, true, errors.New("stopping the original recovery task is unavailable")
	}
	candidate, found, err := s.findRetained(ctx, driver, original)
	if err == nil && !found && (bound == nil || bound.TaskID == "") {
		var confirmed bool
		confirmed, err = confirmNeverAdmitted(ctx, driver, original, requester)
		if confirmed {
			result, stopErr := s.finishRecoveryStop(ctx, target, turn.Result{Text: neverAdmittedMessage(original)}, release)
			return result, true, stopErr
		}
	}
	if err != nil || !found {
		return turn.Result{}, true, errors.Join(harness.ErrStopUnconfirmed, err)
	}
	stopper, ok := driver.(retainedStopDriver)
	if !ok {
		return turn.Result{}, true, errors.New("stopping the original recovery task is unavailable")
	}
	if original.Requester != "" && original.Requester != requester {
		return turn.Result{}, true, errors.New("recovery requires the original requester")
	}
	if bound != nil && bound.TaskID != "" && bound.TaskID != candidate.TaskID {
		return turn.Result{}, true, errors.New("original stop task identity changed")
	}
	s.mu.Lock()
	if controlRecord != nil {
		controlRecord.RecoveryStopTarget = &recoveryStopTarget{Conversation: control.Conversation, ExchangeID: target.ID, TaskID: candidate.TaskID, Requester: requester}
	}
	previousPending, previousTask := target.RecoveryStopPending, target.RecoveryStopTask
	target.RecoveryStopPending, target.RecoveryStopTask = stopRequested, candidate.TaskID
	if err := s.save(); err != nil {
		target.RecoveryStopPending, target.RecoveryStopTask = previousPending, previousTask
		s.mu.Unlock()
		return turn.Result{}, true, err
	}
	s.mu.Unlock()
	result, err := stopper.StopRetainedTask(ctx, candidate.TaskID, turn.Request{Channel: "console", ConversationID: original.Conversation, MessageID: AnchorMark + original.ID, SenderOpenID: requester, ExpectedProject: original.ExpectedProject, Locale: control.Locale}, true)
	if err != nil {
		s.mu.Lock()
		target.RecoveryStopPending = err.Error()
		saveErr := s.save()
		s.mu.Unlock()
		return result, true, errors.Join(err, saveErr)
	}
	result, err = s.finishRecoveryStop(ctx, target, result, release)
	return result, true, err
}

// beginRecoveryStopLocked marks the exchange as stopping, so a second stop
// is refused while this one runs, and returns the release that clears the
// mark once; the mark's channel lets finishing the stop wait on it.
func (s *Service) beginRecoveryStopLocked(target *queuedExchange) (release func()) {
	target.recoveryStopping = make(chan struct{})
	var released sync.Once
	return func() {
		released.Do(func() {
			s.mu.Lock()
			close(target.recoveryStopping)
			target.recoveryStopping = nil
			s.mu.Unlock()
		})
	}
}

// cancelRecovering is the same stop the console's stop control performs,
// reached from the recovery question instead. It runs beside the recovery
// worker, never inside it: finishing a stop waits for that worker to
// release the exchange. It reports what kept the stop from finishing.
func (s *Service) cancelRecovering(ctx context.Context, target *queuedExchange, requester string) error {
	s.mu.Lock()
	if target.RecoveryStop != nil {
		s.mu.Unlock()
		return nil
	}
	if target.recoveryStopping != nil {
		s.mu.Unlock()
		return errors.New("the original recovery task is already stopping")
	}
	if requester == "" || requester != s.owner || (target.Requester != "" && target.Requester != requester) {
		s.mu.Unlock()
		return errors.New("recovery requires the original requester")
	}
	if target.State.Terminal() {
		s.mu.Unlock()
		return errors.New("original stop target has already finished without a stop receipt")
	}
	driver := s.recoveryDriver
	release := s.beginRecoveryStopLocked(target)
	defer release()
	original := copyExchange(target.Exchange)
	s.mu.Unlock()
	if driver == nil {
		return errors.New("stopping the original recovery task is unavailable")
	}
	candidate, found, err := s.findRetained(ctx, driver, original)
	if err == nil && !found {
		var confirmed bool
		confirmed, err = confirmNeverAdmitted(ctx, driver, original, requester)
		if confirmed {
			_, stopErr := s.finishRecoveryStop(ctx, target, turn.Result{Text: neverAdmittedMessage(original)}, release)
			return stopErr
		}
	}
	if err != nil || !found {
		return errors.Join(harness.ErrStopUnconfirmed, err)
	}
	stopper, ok := driver.(retainedStopDriver)
	if !ok {
		return errors.New("stopping the original recovery task is unavailable")
	}
	s.mu.Lock()
	previous, previousTask := target.RecoveryStopPending, target.RecoveryStopTask
	target.RecoveryStopPending, target.RecoveryStopTask = stopRequested, candidate.TaskID
	if saveErr := s.save(); saveErr != nil {
		target.RecoveryStopPending, target.RecoveryStopTask = previous, previousTask
		s.mu.Unlock()
		return saveErr
	}
	s.mu.Unlock()
	result, err := stopper.StopRetainedTask(ctx, candidate.TaskID, turn.Request{Channel: "console", ConversationID: original.Conversation, MessageID: AnchorMark + original.ID, SenderOpenID: requester, ExpectedProject: original.ExpectedProject, Locale: original.Locale}, false)
	if err != nil {
		s.mu.Lock()
		target.RecoveryStopPending = err.Error()
		saveErr := s.save()
		s.mu.Unlock()
		return errors.Join(err, saveErr)
	}
	_, err = s.finishRecoveryStop(ctx, target, result, release)
	return err
}

func (s *Service) finishRecoveryStop(ctx context.Context, target *queuedExchange, result turn.Result, release func()) (turn.Result, error) {
	reply := consoleapi.Reply{Text: result.Text, Title: result.Title}
	s.mu.Lock()
	if target.State.Terminal() {
		s.mu.Unlock()
		return result, nil
	}
	// Persist confirmed settlement before interrupting the waiter. A crash in
	// this gap must finish delivery on restart, never ask or execute again.
	previous, pending := target.RecoveryStop, target.RecoveryStopPending
	target.RecoveryStop, target.RecoveryStopPending = &reply, ""
	if err := s.save(); err != nil {
		target.RecoveryStop, target.RecoveryStopPending = previous, pending
		s.mu.Unlock()
		return turn.Result{}, err
	}
	cancel, done := target.cancel, target.done
	if cancel == nil {
		select {
		case <-done:
			// A failed recovery observer already released its reservation.
			// Reserve just the terminal delivery before finish releases it.
			target.done = make(chan struct{})
			done = target.done
			s.running[target.Conversation]++
		default:
		}
	}
	s.mu.Unlock()
	release()
	if cancel != nil {
		cancel()
	} else {
		s.finish(target, reply, nil)
	}
	select {
	case <-done:
		return result, nil
	case <-ctx.Done():
		return turn.Result{}, ctx.Err()
	}
}

// TasksCancelled wakes the recoveries still in progress because a person
// cancelled tasks, so one bound to such a task looks at once instead of at
// its next pass. What it does then goes by the task store, the same as for
// a task set aside by anything else: the wake is only a nudge to look.
func (s *Service) TasksCancelled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, list := range s.exchanges {
		for _, e := range list {
			if e.State.Terminal() {
				continue
			}
			select {
			case e.taskCancels <- struct{}{}:
			default:
			}
		}
	}
}

// abandon turns the recovery of an exchange whose task, taskID, was set
// aside into waiting for the original execution to stop. It records the
// stop request and its task first, so a restart goes on waiting for the
// stop instead of offering to resume the task again, then tries the stop
// at once and, until it is confirmed, holds the exchange on the card that
// says the stop is being confirmed; a stop already confirmed is simply
// delivered.
func (r *exchangeRecovery) abandon(taskID string) {
	r.stream.Close()
	r.s.mu.Lock()
	if r.e.RecoveryStop == nil && r.e.RecoveryStopPending == "" {
		r.e.RecoveryStopPending, r.e.RecoveryStopTask = stopRequested, taskID
		if err := r.s.save(); err != nil {
			r.e.RecoveryStopPending, r.e.RecoveryStopTask = "", ""
			r.s.mu.Unlock()
			r.s.detachRecovery(r.e, err)
			return
		}
	}
	r.s.mu.Unlock()
	r.s.waitRecoveryStop(r.e, true)
}

// stopSettling reports whether an exchange only waits on the stop of a task
// the owner set aside: its stop was asked for and is not yet confirmed, and
// the task store has the task cancelled or paused. Nothing is left for it
// to do with the conversation — the task can no longer be resumed through
// it, and settling the stop needs neither the line nor the conversation's
// tasks — so it holds neither the conversation's queue nor the end of the
// conversation's tasks. Its card stays until the stop is confirmed.
func stopSettling(tx ledger.Reader, state consoleapi.ExchangeState, recoveryPending bool, stopPending, stopTask string) (bool, error) {
	if (state != consoleapi.ExchangeRecovering && state != consoleapi.ExchangeAwaitingUser) || recoveryPending || stopPending == "" || stopTask == "" {
		return false, nil
	}
	tracked, found, err := task.GetTx(tx, stopTask)
	if err != nil || !found {
		return false, err
	}
	return setAside(tracked.State), nil
}

// stopSettlingLocked is stopSettling for e as the task store has it now. A
// console without a ledger cannot tell, so its exchanges hold the line as
// before, and so does one whose task cannot be read.
func (s *Service) stopSettlingLocked(e *queuedExchange) bool {
	if s.book == nil || e.RecoveryStopPending == "" || e.RecoveryStopTask == "" {
		return false
	}
	var settling bool
	err := s.book.Read(context.Background(), func(tx *ledger.ReadTx) error {
		var err error
		settling, err = stopSettling(tx, e.State, e.RecoveryPending, e.RecoveryStopPending, e.RecoveryStopTask)
		return err
	})
	if err != nil {
		slog.Error("console: read the task of a pending stop", "conversation", e.Conversation, "exchange", e.ID, "task", e.RecoveryStopTask, "error", err)
		return false
	}
	return settling
}

// A parent observer can finish before the stop of one of its children is
// confirmed. waitRecoveryStop keeps that stop moving instead of parking the
// exchange on a card the owner can only read: it rechecks on its own cadence,
// says what is still unconfirmed, and settles the exchange the moment the
// stop confirms — including when the durable task-stop reconciliation running
// behind the console confirmed it while nobody was watching. checkNow makes
// the first check at once instead of a cadence later.
func (s *Service) waitRecoveryStop(e *queuedExchange, checkNow bool) {
	s.mu.Lock()
	if e.RecoveryStop != nil {
		reply := *e.RecoveryStop
		s.mu.Unlock()
		s.finish(e, reply, nil)
		return
	}
	requester := s.owner
	if e.Requester != "" {
		requester = e.Requester
	}
	w := &recoveryStopWait{s: s, e: e, ctx: e.ctx, requester: requester, reason: e.RecoveryStopPending, changed: make(chan struct{}, 1)}
	e.State = consoleapi.ExchangeAwaitingUser
	saveErr := s.save()
	if saveErr == nil {
		// Waiting on the stop of a task set aside, the exchange no longer
		// holds the line; what is queued behind it goes ahead.
		if err := s.startNextLocked(e.Conversation); err != nil {
			slog.Error("console: start the line past a pending stop", "conversation", e.Conversation, "exchange", e.ID, "error", err)
		}
	}
	s.publishQueue(e.Conversation)
	w.base = consoleapi.PendingQuestion{Conversation: e.Conversation, ExchangeID: e.ID, Project: e.ExpectedProject, Locale: e.Locale}
	s.mu.Unlock()
	if saveErr != nil {
		s.detachRecovery(e, saveErr)
		return
	}
	if checkNow {
		w.check()
	}
	w.run()
}

// recoveryStopEvery is how often an unconfirmed stop is checked again
// without the owner asking. It is long enough not to hammer a node that is
// away, short enough that a stop confirmed elsewhere settles the exchange
// while they are still looking at it.
const recoveryStopEvery = 30 * time.Second

// recoveryStopWait drives an already requested stop to a confirmed end.
type recoveryStopWait struct {
	s         *Service
	e         *queuedExchange
	ctx       context.Context
	base      consoleapi.PendingQuestion
	requester string
	changed   chan struct{}

	// reason is why the stop has not finished yet and asked is the reason
	// the owner last saw, so an unchanged answer rechecks quietly instead
	// of posting the same card again. checks counts the passes made for
	// them. The periodic pass writes all three, so they are held.
	mu     sync.Mutex
	reason string
	asked  string
	checks int
	busy   bool
}

func (w *recoveryStopWait) run() {
	stop := w.poll()
	defer stop()
	for {
		if w.ctx.Err() != nil {
			w.s.detachRecovery(w.e, w.ctx.Err())
			return
		}
		choice, ok := w.ask()
		if !ok {
			return
		}
		if choice == "recheck" {
			w.check()
			continue
		}
		if !w.quiet() {
			return
		}
	}
}

// poll rechecks on its own cadence for as long as the exchange lives. It is
// what makes an unattended stop finish: the owner can close the page and the
// turn still ends when the stop is confirmed.
func (w *recoveryStopWait) poll() func() {
	ctx, cancel := context.WithCancel(w.ctx)
	w.s.workers.Add(1)
	go func() {
		defer w.s.workers.Done()
		ticker := time.NewTicker(w.s.recoveryStopEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.check()
			case <-ctx.Done():
				return
			}
		}
	}()
	return cancel
}

// check runs one stop pass unless one is already running. The pass runs
// beside this wait because confirming a stop waits for the wait to release
// the exchange.
func (w *recoveryStopWait) check() {
	w.mu.Lock()
	if w.busy || w.ctx.Err() != nil {
		w.mu.Unlock()
		return
	}
	w.busy, w.checks = true, w.checks+1
	w.mu.Unlock()
	ctx := w.s.exchangeContext(w.ctx)
	w.s.workers.Add(1)
	go func() {
		defer w.s.workers.Done()
		err := w.s.cancelRecovering(ctx, w.e, w.requester)
		// The pass may have been what set the task aside, which lets the
		// line move past this exchange.
		w.s.mu.Lock()
		if !w.e.State.Terminal() {
			if startErr := w.s.startNextLocked(w.e.Conversation); startErr != nil {
				slog.Error("console: start the line past a pending stop", "conversation", w.e.Conversation, "exchange", w.e.ID, "error", startErr)
			}
		}
		w.s.mu.Unlock()
		w.mu.Lock()
		w.busy = false
		if err != nil {
			w.reason = clipDetail(strings.TrimSpace(err.Error()))
		}
		fresh := w.reason != w.asked
		w.mu.Unlock()
		if fresh {
			select {
			case w.changed <- struct{}{}:
			default:
			}
		}
	}()
}

// quiet holds until there is something new to say or the exchange's
// lifetime ends. The owner asked to be left alone until then.
func (w *recoveryStopWait) quiet() bool {
	for {
		select {
		case <-w.changed:
			w.mu.Lock()
			fresh := w.reason != w.asked
			w.mu.Unlock()
			if fresh {
				return true
			}
		case <-w.ctx.Done():
			w.s.detachRecovery(w.e, w.ctx.Err())
			return false
		}
	}
}

// ask reports the stop's state and offers the two moves that mean anything
// here: check right now, or let the recheck run itself.
func (w *recoveryStopWait) ask() (string, bool) {
	text := i18n.New(i18n.FromLang(w.base.Locale))
	paused := w.paused()
	w.mu.Lock()
	w.asked = w.reason
	message := w.messageLocked(text, paused)
	w.mu.Unlock()
	question := view.Question{
		Kind:          "recovery",
		Required:      true,
		AllowFreeText: true,
		Title:         text.T(i18n.ConsoleStopWaitTitle),
		Message:       message,
		Choices: []view.Choice{
			{Value: "recheck", Label: text.T(i18n.ConsoleStopRecheckNow), Detail: text.T(i18n.ConsoleStopRecheckNowDetail)},
			{Value: "wait", Label: text.T(i18n.ConsoleStopLetItCheck), Detail: text.T(i18n.ConsoleStopLetItCheckDetail)},
		},
	}
	answer, err := w.s.askUser(w.ctx, w.base, question, false)
	if err != nil {
		if w.ctx.Err() != nil {
			err = w.ctx.Err()
		}
		w.s.detachRecovery(w.e, err)
		return "", false
	}
	return answer.Value, true
}

// paused reports whether the task store has the task whose execution this
// waits on paused rather than cancelled, so the card says the task stays
// paused instead of calling it stopped.
func (w *recoveryStopWait) paused() bool {
	w.s.mu.Lock()
	driver := w.s.recoveryDriver
	original := copyExchange(w.e.Exchange)
	w.s.mu.Unlock()
	if driver == nil {
		return false
	}
	candidate, found, err := w.s.findRetained(w.ctx, driver, original)
	return err == nil && found && candidate.TaskState == task.StatePaused
}

func (w *recoveryStopWait) messageLocked(text i18n.Catalog, paused bool) string {
	message := text.T(i18n.ConsoleStopRecorded)
	if paused {
		message = text.T(i18n.ConsolePauseRecorded)
	}
	if w.reason != "" {
		message += "\n\n" + text.T(i18n.ConsoleStopUnconfirmed, w.reason)
	}
	message += "\n\n"
	if w.checks > 0 {
		message += text.T(i18n.ConsoleStopChecked, w.checks)
	}
	return message + text.T(i18n.ConsoleStopPreserved)
}
