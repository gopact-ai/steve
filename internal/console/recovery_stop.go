package console

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type retainedStopDriver interface {
	StopRetainedTask(context.Context, string, turn.Request) (turn.Result, error)
}

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
		if e.ID == control.ID || (bound != nil && e.ID != bound.ExchangeID) || (bound == nil && e.State != consoleapi.ExchangeRecovering && e.State != consoleapi.ExchangeAwaitingUser) {
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
	previousPending := target.RecoveryStopPending
	target.RecoveryStopPending = "Stop requested; waiting for confirmation from the original task and its children."
	if err := s.save(); err != nil {
		target.RecoveryStopPending = previousPending
		s.mu.Unlock()
		return turn.Result{}, true, err
	}
	s.mu.Unlock()
	result, err := stopper.StopRetainedTask(ctx, candidate.TaskID, turn.Request{Channel: "console", ConversationID: original.Conversation, MessageID: AnchorMark + original.ID, SenderOpenID: requester, ExpectedProject: original.ExpectedProject, Locale: control.Locale})
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
	previous := target.RecoveryStopPending
	target.RecoveryStopPending = "Stop requested; waiting for confirmation from the original task and its children."
	if saveErr := s.save(); saveErr != nil {
		target.RecoveryStopPending = previous
		s.mu.Unlock()
		return saveErr
	}
	s.mu.Unlock()
	result, err := stopper.StopRetainedTask(ctx, candidate.TaskID, turn.Request{Channel: "console", ConversationID: original.Conversation, MessageID: AnchorMark + original.ID, SenderOpenID: requester, ExpectedProject: original.ExpectedProject, Locale: original.Locale})
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

// A parent observer can finish before the stop of one of its children is
// confirmed. waitRecoveryStop keeps that stop moving instead of parking the
// exchange on a card the owner can only read: it rechecks on its own cadence,
// says what is still unconfirmed, and settles the exchange the moment the
// stop confirms — including when the durable task-stop reconciliation running
// behind the console confirmed it while nobody was watching.
func (s *Service) waitRecoveryStop(e *queuedExchange) {
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
	s.publishQueue(e.Conversation)
	w.base = consoleapi.PendingQuestion{Conversation: e.Conversation, ExchangeID: e.ID, Project: e.ExpectedProject, Locale: e.Locale}
	s.mu.Unlock()
	if saveErr != nil {
		s.detachRecovery(e, saveErr)
		return
	}
	w.run()
}

// recoveryStopInterval is how often an unconfirmed stop is checked again
// without the owner asking. It is long enough not to hammer a node that is
// away, short enough that a stop confirmed elsewhere settles the exchange
// while they are still looking at it.
// It is a variable so a test can shorten it, and atomic because the
// pollers a finished test leaves behind still read it while the next one
// sets its own.
var recoveryStopInterval atomic.Int64

func init() { recoveryStopInterval.Store(int64(30 * time.Second)) }

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
		ticker := time.NewTicker(time.Duration(recoveryStopInterval.Load()))
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
	en := w.base.Locale == "en"
	w.mu.Lock()
	w.asked = w.reason
	message := w.messageLocked(en)
	w.mu.Unlock()
	question := view.Question{
		Kind:          "recovery",
		Required:      true,
		AllowFreeText: true,
		Title:         line(en, "正在核实任务是否已停止", "Confirming the task has stopped"),
		Message:       message,
		Choices: []view.Choice{
			{Value: "recheck", Label: line(en, "立刻再核实一次", "Check again now"), Detail: line(en, "马上重新联系原节点核对这次执行。", "Contact the original node again right now.")},
			{Value: "wait", Label: line(en, "交给它自己核实", "Let it keep checking"), Detail: line(en, "到点自动核实，确认后这一回合会自己结束。", "It rechecks on its own and ends this turn once the stop is confirmed.")},
		},
	}
	answer, err := w.s.askUser(w.ctx, w.base, question)
	if err != nil {
		if w.ctx.Err() != nil {
			err = w.ctx.Err()
		}
		w.s.detachRecovery(w.e, err)
		return "", false
	}
	return answer.Value, true
}

func (w *recoveryStopWait) messageLocked(en bool) string {
	message := line(en,
		"停止要求已经记录，正在核对原任务和它的子任务是否真的停下来了。",
		"The stop request is recorded and the original task and its children are being checked.")
	if w.reason != "" {
		message += line(en, "\n\n目前还没确认停止的是：\n", "\n\nStill unconfirmed:\n") + w.reason
	}
	if w.checks > 0 {
		message += line(en, "\n\n已经核对 ", "\n\nChecked ") + strconv.Itoa(w.checks) + line(en, " 次。", " times so far.")
	} else {
		message += line(en, "\n\n", "\n\n")
	}
	message += line(en,
		"任务和待发指令都保留着，不会丢。系统会按固定间隔自己再核对一次，确认停止后这一回合会自动结束，你不需要一直守着。",
		"The task and its queued instructions are preserved. Steve rechecks at a fixed interval on its own and ends this turn once the stop is confirmed, so you do not need to watch it.")
	return message
}
