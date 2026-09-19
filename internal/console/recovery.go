package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

// RetainedChatDriver observes already accepted work without invoking Handle.
type RetainedChatDriver interface {
	RetainedChats(context.Context) ([]turn.RetainedChat, error)
	ResumeRetainedChat(context.Context, string, turn.Request) (turn.Result, error)
}

type retainedPlanDriver interface {
	RetainedPlans(context.Context) ([]turn.RetainedPlan, error)
	ResumeRetainedPlan(context.Context, turn.RetainedPlan, turn.Request) (turn.Result, error)
}

type retainedExchange struct {
	turn.RetainedChat
	plan *turn.RetainedPlan
}

// EnableRetainedRecovery is wired before Persist for a coordinator generation.
// Standalone consoles keep their existing interruption behavior.
func (s *Service) EnableRetainedRecovery(lifetime context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoveryLifetime = lifetime
}

func (s *Service) recoveryStoppedLocked() bool {
	return s.recoveryLifetime != nil && (s.closing || s.recoveryLifetime.Err() != nil)
}

type exchangeLifetime struct {
	context.Context
	values context.Context
}

func (c exchangeLifetime) Value(key any) any { return c.values.Value(key) }
func (s *Service) exchangeContext(ctx context.Context) context.Context {
	if s.recoveryLifetime != nil {
		return exchangeLifetime{Context: s.recoveryLifetime, values: ctx}
	}
	return context.WithoutCancel(ctx)
}

// RecoverChats reserves each original exchange before Drain can start queued
// input. A missing native session produces an explicit recovery question, not
// an invented reply or a second submission of the original prompt.
func (s *Service) RecoverChats(ctx context.Context, driver RetainedChatDriver) error {
	if driver == nil {
		return errors.New("retained recovery driver is required")
	}
	if _, err := driver.RetainedChats(ctx); err != nil {
		return err
	}
	if plans, ok := driver.(retainedPlanDriver); ok {
		if _, err := plans.RetainedPlans(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryLifetime == nil {
		return errors.New("retained recovery must be enabled before loading the console")
	}
	if s.recoveryStoppedLocked() {
		return consoleapi.ErrConsoleClosing
	}
	s.recoveryDriver = driver
	for _, list := range s.exchanges {
		for _, e := range list {
			if (e.State != consoleapi.ExchangeRecovering && e.State != consoleapi.ExchangeAwaitingUser) || e.cancel != nil {
				continue
			}
			ctx, cancel := context.WithCancel(s.exchangeContext(ctx))
			e.ctx, e.cancel = ctx, cancel
			s.workers.Add(1)
			go func() { defer s.workers.Done(); defer cancel(); s.recoverExchange(ctx, e, driver) }()
		}
	}
	return nil
}

// RequestRecovery asks a platform reconciliation question in its original
// conversation. The authenticated console owner supplies the principal; this
// call cannot forge a native tool callback or grant execution permission.
func (s *Service) RequestRecovery(ctx context.Context, binding consoleapi.PendingQuestion, question view.Question) (view.Answer, error) {
	if !IsConsole(binding.Conversation) || binding.TaskID == "" || binding.AttemptID == "" || s.owner == "" {
		return view.Answer{}, consoleapi.ErrInvalidAnswer
	}
	s.mu.Lock()
	if s.recoveryStoppedLocked() {
		s.mu.Unlock()
		return view.Answer{}, consoleapi.ErrConsoleClosing
	}
	if binding.ExchangeID != "" {
		found := false
		for _, exchange := range s.exchanges[binding.Conversation] {
			if exchange.ID == binding.ExchangeID {
				found = true
				break
			}
		}
		if !found {
			s.mu.Unlock()
			return view.Answer{}, consoleapi.ErrExchangeNotFound
		}
	}
	s.mu.Unlock()
	question.Kind = "recovery"
	question.SessionID = binding.SessionID
	question.Generation = binding.Generation
	return s.askUser(ctx, consoleapi.PendingQuestion{Conversation: binding.Conversation, ExchangeID: binding.ExchangeID, Project: binding.Project, TaskID: binding.TaskID, AttemptID: binding.AttemptID, SessionID: binding.SessionID, Generation: binding.Generation, Locale: binding.Locale}, question)
}

// continueDetached preserves a managed exchange when its observer fails while
// this coordinator remains healthy. Native state is reconciled through the
// same startup consumer, never by sending the original prompt a second time.
func (s *Service) continueDetached(e *queuedExchange, err error) bool {
	if !errors.Is(err, harness.ErrStopUnconfirmed) && !isRecoveryBlocked(err) {
		return false
	}
	s.mu.Lock()
	driver := s.recoveryDriver
	enabled := s.recoveryLifetime != nil && !s.recoveryStoppedLocked()
	exchange := copyExchange(e.Exchange)
	s.mu.Unlock()
	if !enabled || driver == nil {
		return false
	}
	_, found, lookupErr := s.findRetained(s.recoveryLifetime, driver, exchange)
	if lookupErr != nil || !found {
		return false
	}
	s.mu.Lock()
	if s.recoveryStoppedLocked() {
		s.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(s.exchangeContext(e.ctx))
	e.ctx, e.cancel = ctx, cancel
	e.State = consoleapi.ExchangeRecovering
	if err := s.save(); err != nil {
		s.detachRecoveryLocked(e, err)
		s.mu.Unlock()
		cancel()
		return true
	}
	s.publishQueue(e.Conversation)
	s.mu.Unlock()
	defer cancel()
	s.recoverExchange(ctx, e, driver)
	return true
}

func (s *Service) findRetained(ctx context.Context, driver RetainedChatDriver, e Exchange) (retainedExchange, bool, error) {
	items, err := driver.RetainedChats(ctx)
	if err != nil {
		return retainedExchange{}, false, err
	}
	var found retainedExchange
	ok := false
	for _, item := range items {
		if item.Conversation == e.Conversation && item.MessageID == AnchorMark+e.ID {
			if ok && item.AttemptID != found.AttemptID {
				return retainedExchange{}, false, errors.New("multiple executions reference the same exchange")
			}
			found, ok = retainedExchange{RetainedChat: item}, true
		}
	}
	if plans, supported := driver.(retainedPlanDriver); supported {
		items, err := plans.RetainedPlans(ctx)
		if err != nil {
			return retainedExchange{}, false, err
		}
		for _, item := range items {
			if item.Conversation != e.Conversation || item.MessageID != AnchorMark+e.ID {
				continue
			}
			if ok {
				return retainedExchange{}, false, errors.New("multiple chat or plan executions reference the same exchange")
			}
			found = retainedExchange{RetainedChat: turn.RetainedChat{AttemptID: item.AttemptID, TaskID: item.TaskID, Conversation: item.Conversation, MessageID: item.MessageID, AgentID: item.AgentID, NodeID: item.NodeID, ProjectID: item.ProjectID, Completed: item.Completed}, plan: &item}
			ok = true
		}
	}
	return found, ok, nil
}

// recoveryQuiet is how long a recovery keeps rejoining the original
// execution on its own before anything is put to the owner. A node
// restart or a dropped link lasts seconds and the execution keeps
// running through it; asking about that is noise, not news.
const recoveryQuiet = 90 * time.Second

// recoveryRetry is how often a quiet recovery looks again, and
// recoveryProbe how often an open recovery question checks whether the
// original execution can be reached once more.
const (
	recoveryRetry = 5 * time.Second
	recoveryProbe = 10 * time.Second
)

// retainedProber reports whether a retained execution can be reached
// again, reading only the node's view of its own session. Nothing is
// adopted, settled or replayed, so a recovery that is waiting on an
// answer can use it to notice the original coming back.
type retainedProber interface {
	ProbeRetained(context.Context, string) error
}

// exchangeRecovery is one worker reattaching a queued exchange to the
// execution it was interrupted from. Each pass observes the retained work
// afresh; quiet and waitingPlan carry the owner's last answer from one
// pass to the next, so an unresolved question is not asked again until
// something changes.
type exchangeRecovery struct {
	s        *Service
	ctx      context.Context
	e        *queuedExchange
	exchange Exchange
	work     *process
	stream   *progressStream
	driver   RetainedChatDriver

	// quiet holds after the owner declined to retry: the loop then waits
	// and re-observes instead of asking again. waitingPlan is the
	// relocation plan they were asked about, so a different plan asks anew.
	quiet       bool
	waitingPlan string

	// asked and repeats remember the block the owner last saw, so a check
	// that comes back with the same answer says so instead of looking
	// like fresh news. stopped carries back why a stop the owner asked
	// for did not finish; it is written by the stop's own goroutine.
	asked   string
	repeats int
	stopped chan string

	// blockedSince is when this recovery first failed to rejoin the
	// original without saying anything, and rejoin records that an open
	// question was withdrawn because the original came back. Together
	// they keep a brief outage between the recovery and the node.
	blockedSince time.Time
	rejoin       bool
}

func (s *Service) recoverExchange(ctx context.Context, e *queuedExchange, driver RetainedChatDriver) {
	exchange, work, ok := s.openRecovery(e)
	if !ok {
		return
	}
	stream := s.progress(exchange.Conversation, exchange.ID, work)
	defer stream.Close()
	stop := s.follow(ctx, exchange.Conversation, work)
	defer stop()
	if s.anchor != nil {
		s.anchor(exchange.Conversation, ChatID, AnchorMark+exchange.ID)
	}
	r := &exchangeRecovery{s: s, ctx: ctx, e: e, exchange: exchange, work: work, stream: stream, driver: driver, stopped: make(chan string, 1)}
	for {
		if ctx.Err() != nil {
			r.detach(ctx.Err())
			return
		}
		if !r.observe() {
			return
		}
	}
}

// openRecovery registers the exchange's process so the page can follow
// the recovery. An exchange already stopped while recovering settles with
// its stop reply instead, and one whose stop is still pending waits for
// it; neither gets a worker.
func (s *Service) openRecovery(e *queuedExchange) (Exchange, *process, bool) {
	s.mu.Lock()
	if e.RecoveryStop != nil {
		reply := *e.RecoveryStop
		s.mu.Unlock()
		s.finish(e, reply, nil)
		return Exchange{}, nil, false
	}
	if e.RecoveryStopPending != "" {
		s.mu.Unlock()
		s.waitRecoveryStop(e)
		return Exchange{}, nil, false
	}
	exchange := copyExchange(e.Exchange)
	work := newProcess()
	if s.processes == nil {
		s.processes = map[string]*process{}
	}
	s.processes[e.ID] = work
	s.mu.Unlock()
	return exchange, work, true
}

// markExchange persists a recovery state change and tells the page.
func (s *Service) markExchange(e *queuedExchange, state consoleapi.ExchangeState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.State = state
	err := s.save()
	s.publishQueue(e.Conversation)
	return err
}

// observe makes one pass: it looks for the retained execution, resumes or
// relocates it when it can, and otherwise puts what blocks the recovery
// to the owner. It reports whether another pass should follow.
func (r *exchangeRecovery) observe() bool {
	candidate, found, lookupErr := r.s.findRetained(r.ctx, r.driver, r.exchange)
	identity := r.identity(candidate, found)
	requester := r.s.owner
	if r.exchange.Requester != "" {
		requester = r.exchange.Requester
	}
	if requester == "" || requester != r.s.owner {
		r.stream.Close()
		r.s.finish(r.e, consoleapi.Reply{}, errors.New("recovery requires the original console owner"))
		return false
	}
	request := r.request(requester, identity)
	var err error
	if found && lookupErr == nil {
		var done bool
		if done, err = r.resume(candidate, request); done {
			return false
		}
	}
	if r.ctx.Err() != nil {
		r.detach(r.ctx.Err())
		return false
	}
	// A pass that got this far could not rejoin the original. For the
	// first stretch that is a node coming back, not something the owner
	// has to decide, so the recovery keeps trying by itself.
	if !r.quiet && r.withinQuiet() {
		return r.waitFor(min(recoveryRetry, r.s.recoveryQuiet))
	}
	if planner, ok := r.driver.(relocationDriver); ok && found && candidate.plan == nil && isRecoveryBlocked(err) {
		var done bool
		done, err = r.relocate(planner, candidate, request, identity, err)
		if done {
			return false
		}
		if r.rejoined() {
			return true
		}
	}
	return r.consult(blockedBy(err, lookupErr), identity)
}

// identity binds the pass's questions to the execution found for the
// exchange; a resumed turn refines it through OnTurnReady.
func (r *exchangeRecovery) identity(candidate retainedExchange, found bool) *questionIdentity {
	base := consoleapi.PendingQuestion{Conversation: r.exchange.Conversation, ExchangeID: r.exchange.ID, Project: r.exchange.ExpectedProject, TaskID: candidate.TaskID, AttemptID: candidate.AttemptID, Locale: r.exchange.Locale}
	if found && candidate.ProjectID != "" {
		base.Project = candidate.ProjectID
	}
	return &questionIdentity{base: base}
}

// request is the turn request a resumed or relocated execution answers,
// with its progress and questions routed to this exchange.
func (r *exchangeRecovery) request(requester string, identity *questionIdentity) turn.Request {
	return turn.Request{Channel: "console", ConversationID: r.exchange.Conversation, MessageID: AnchorMark + r.exchange.ID, ChatID: ChatID, SenderOpenID: requester, ChatType: protocol.ChatP2P, Mentioned: true, Origin: r.exchange.Origin, ExpectedProject: r.exchange.ExpectedProject, ExpectedTask: r.exchange.ExpectedTask, Locale: r.exchange.Locale,
		OnTurnReady: identity.set,
		OnProgress:  r.stream.Update,
		OnPhase:     r.stream.Phase,
		OnStage:     r.stream.Stage,
		OnAsk: func(ctx context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			return r.s.askPermission(ctx, identity.binding(), ask)
		},
		OnAskUser: func(ctx context.Context, q view.Question) (view.Answer, error) {
			return r.s.askUser(ctx, identity.binding(), q)
		},
	}
}

// resume reattaches to the retained execution once the exchange is
// recorded as recovering. It reports whether the worker is done — the
// exchange settled, or a cancelled console detached it — and otherwise
// what now blocks the recovery: the state save that failed before the
// resumption was tried, or what the resumption itself hit.
func (r *exchangeRecovery) resume(candidate retainedExchange, request turn.Request) (done bool, blocked error) {
	// Do not reattach behind a state the console could not write: an
	// execution resumed under an unrecorded exchange would be lost again
	// by the next restart, so the failure is what the owner is told.
	if saveErr := r.s.markExchange(r.e, consoleapi.ExchangeRecovering); saveErr != nil {
		if r.ctx.Err() != nil {
			r.detach(r.ctx.Err())
			return true, nil
		}
		return false, saveErr
	}
	var result turn.Result
	var err error
	if candidate.plan != nil {
		result, err = r.driver.(retainedPlanDriver).ResumeRetainedPlan(r.ctx, *candidate.plan, request)
	} else {
		result, err = r.driver.ResumeRetainedChat(r.ctx, candidate.AttemptID, request)
	}
	if settles(result, err) {
		r.settle(result, err)
		return true, nil
	}
	return false, err
}

// relocate moves the blocked chat to another node by a plan the owner
// approves, unless they already did or the plan needs no approval. It
// reports whether the exchange settled; otherwise it returns what now
// blocks the recovery, which stays the incoming block when planning
// changed nothing.
func (r *exchangeRecovery) relocate(planner relocationDriver, candidate retainedExchange, request turn.Request, identity *questionIdentity, blocked error) (bool, error) {
	input, images, captureErr := r.s.relocationInput(r.ctx, r.exchange, candidate.ProjectID, request.SenderOpenID)
	if captureErr != nil {
		return false, blockedRecovery(captureErr, "恢复上下文需要处理", "已读取原输入与冻结材料。\n\n当前无法完整重建恢复上下文："+captureErr.Error()+"\n\n不会用新文件或猜测替换原材料。\n\n建议恢复原材料后重新检查。", "重新检查")
	}
	request.Relocation, request.Images = input, images
	plan, planErr := planner.PlanRelocation(r.ctx, candidate.AttemptID, request)
	if planErr != nil || plan.ID == "" {
		if isRecoveryBlocked(planErr) {
			return false, planErr
		}
		return false, blocked
	}
	signature := plan.ID + "/" + plan.Question.Message
	if r.quiet && r.waitingPlan != "" && signature != r.waitingPlan {
		r.quiet = false
	}
	if r.quiet {
		return false, blocked
	}
	choice := r.s.approvedRelocation(identity.binding(), plan.ID)
	if !plan.Automatic && !plan.Approved && choice == "" {
		done, approved := r.approvePlan(plan, signature, identity)
		if done {
			return true, nil
		}
		if approved == "" {
			return false, blocked
		}
		choice = approved
	}
	result, err := planner.RelocateChat(r.ctx, plan.ID, choice, request)
	if settles(result, err) {
		r.settle(result, err)
		return true, nil
	}
	return false, blockedRecovery(err, "恢复方案需要重新检查", "已按确认的恢复方案检查执行条件。\n\n本次恢复尚未完成："+err.Error()+"\n\n已保存的原任务和恢复记录仍保留，没有将失败当作完成。\n\n建议重新检查当前执行，或等待原节点恢复。", "重新检查")
}

// approvePlan records the exchange as waiting and puts the relocation plan
// to the owner. It reports whether the worker is done — the save or the
// question failed and the exchange was detached — and otherwise the
// approved choice; any other answer leaves the recovery quiet on this
// plan and the choice empty.
func (r *exchangeRecovery) approvePlan(plan turn.RelocationPlan, signature string, identity *questionIdentity) (done bool, choice string) {
	if err := r.s.markExchange(r.e, consoleapi.ExchangeAwaitingUser); err != nil {
		r.detach(err)
		return true, ""
	}
	answer, err := r.ask(identity, plan.Question, r.s.RequestRecovery)
	if err != nil {
		r.detach(err)
		return true, ""
	}
	if r.rejoin {
		return false, ""
	}
	if answer.Value != "confirm-stopped-and-retry:"+plan.ID {
		r.quiet, r.waitingPlan = true, signature
		return false, ""
	}
	return false, answer.Value
}

// consult records the block for the owner. While they have declined to
// retry it waits and observes again; otherwise it asks, and the answer
// decides whether the next pass asks once more. It reports whether
// another pass should follow.
func (r *exchangeRecovery) consult(blocked *turn.RecoveryBlocked, identity *questionIdentity) bool {
	if err := r.s.markExchange(r.e, consoleapi.ExchangeAwaitingUser); err != nil {
		if r.ctx.Err() != nil {
			err = r.ctx.Err()
		}
		r.detach(err)
		return false
	}
	if r.quiet {
		return r.wait()
	}
	question := blocked.Question
	question.Kind = "recovery"
	if question.Message == r.asked {
		r.repeats++
	} else {
		r.asked, r.repeats = question.Message, 1
	}
	question.Message = r.explain(question.Message, blocked.Cause)
	question.Choices = append(append([]view.Choice{}, question.Choices...), stopChoice(r.exchange.Locale))
	answer, err := r.ask(identity, question, r.s.askUser)
	if err != nil {
		if r.ctx.Err() != nil {
			err = r.ctx.Err()
		}
		r.detach(err)
		return false
	}
	// The original came back while the card was open: it was withdrawn
	// and this pass rejoins the execution instead of waiting on an
	// answer nobody needs to give any more.
	if r.rejoined() {
		return true
	}
	// Stopping is the one answer that can end a recovery nothing else can
	// resolve. It runs beside this worker, because finishing a stop waits
	// for this worker to release the exchange.
	if answer.Value == "stop" {
		r.stopOriginal()
		r.quiet = true
		return r.wait()
	}
	// Free-form advice is retained in the question record. Without an
	// explicit retry choice, make one fresh observation and then wait
	// quietly instead of repeatedly asking the same unresolved question.
	r.quiet = answer.Value != "retry"
	return true
}

// explain adds to a block what the owner needs in order to decide: the
// failure the check actually reported, whether this check said anything
// new, and what became of a stop they already asked for.
func (r *exchangeRecovery) explain(message string, cause error) string {
	en := r.exchange.Locale == "en"
	select {
	case note := <-r.stopped:
		if note != "" {
			message += line(en, "\n\n上一次「停止并取消原执行」没有完成：", "\n\nThe last stop did not finish: ") + note
		}
	default:
	}
	if cause != nil {
		if detail := clipDetail(strings.TrimSpace(cause.Error())); detail != "" && !strings.Contains(message, detail) {
			message += line(en, "\n\n本次检查报告的失败：", "\n\nWhat the check reported: ") + detail
		}
	}
	if r.repeats > 1 {
		message += line(en, "\n\n这已经是第 ", "\n\nThis is check number ") + strconv.Itoa(r.repeats) +
			line(en, " 次检查，结果与上一次相同。再检查一次大概率还是同样的结果；要结束这一回合，选择「停止并取消原执行」。", ", and it came back the same as the last one. Another check is unlikely to differ; to end this turn, choose to stop and cancel the original run.")
	}
	return message
}

func line(en bool, zh, english string) string {
	if en {
		return english
	}
	return zh
}

// clipDetail keeps a reported failure readable in a question card.
func clipDetail(detail string) string {
	const limit = 300
	runes := []rune(detail)
	if len(runes) <= limit {
		return detail
	}
	return string(runes[:limit]) + "…"
}

func stopChoice(locale string) view.Choice {
	if locale == "en" {
		return view.Choice{Value: "stop", Label: "Stop and cancel the original run", Detail: "Have the node seal the original open command, confirm the cancellation, and end this turn."}
	}
	return view.Choice{Value: "stop", Label: "停止并取消原执行", Detail: "让节点封存原创建指令并核实取消结果，然后结束这一回合。"}
}

// stopOriginal performs the owner's stop next to this worker and brings
// back why it did not finish, so the next question says so.
func (r *exchangeRecovery) stopOriginal() {
	requester := r.s.owner
	if r.exchange.Requester != "" {
		requester = r.exchange.Requester
	}
	ctx := r.s.exchangeContext(r.ctx)
	r.s.workers.Add(1)
	go func() {
		defer r.s.workers.Done()
		if err := r.s.cancelRecovering(ctx, r.e, requester); err != nil {
			select {
			case r.stopped <- clipDetail(strings.TrimSpace(err.Error())):
			default:
			}
		}
	}()
}

// withinQuiet reports whether this recovery is still inside the stretch
// it works through on its own. The clock starts at the first pass that
// could not rejoin the original and is cleared once one does.
func (r *exchangeRecovery) withinQuiet() bool {
	if r.blockedSince.IsZero() {
		r.blockedSince = time.Now()
	}
	return time.Since(r.blockedSince) < r.s.recoveryQuiet
}

// rejoined consumes a withdrawal made because the original became
// reachable again, and puts the recovery back where it was before
// anything was asked.
func (r *exchangeRecovery) rejoined() bool {
	if !r.rejoin {
		return false
	}
	r.rejoin, r.quiet, r.waitingPlan = false, false, ""
	r.asked, r.repeats, r.blockedSince = "", 0, time.Time{}
	return true
}

// ask puts a question to the owner while watching the original
// execution. A node that comes back withdraws the question, because the
// recovery can then continue without an answer; rejoin says so.
func (r *exchangeRecovery) ask(identity *questionIdentity, question view.Question, put func(context.Context, consoleapi.PendingQuestion, view.Question) (view.Answer, error)) (view.Answer, error) {
	binding := identity.binding()
	prober, ok := r.driver.(retainedProber)
	if !ok || binding.AttemptID == "" {
		return put(r.ctx, binding, question)
	}
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	back, reachable := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(back)
		ticker := time.NewTicker(r.s.recoveryProbe)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := prober.ProbeRetained(ctx, binding.AttemptID); err == nil {
				close(reachable)
				cancel()
				return
			}
		}
	}()
	answer, err := put(ctx, binding, question)
	cancel()
	<-back
	// An answer given at the same moment is still the owner's decision;
	// only a question nobody answered is withdrawn.
	select {
	case <-reachable:
		if err == nil && r.ctx.Err() == nil && answer.Value == "" && answer.Text == "" && answer.Decision == "" {
			r.rejoin = true
			return view.Answer{}, nil
		}
	default:
	}
	return answer, err
}

// wait lets a quiet recovery observe the execution again every so often,
// until the exchange's lifetime ends.
func (r *exchangeRecovery) wait() bool { return r.waitFor(30 * time.Second) }

func (r *exchangeRecovery) waitFor(d time.Duration) bool {
	timer := time.NewTimer(d)
	select {
	case <-timer.C:
		return true
	case <-r.ctx.Done():
		timer.Stop()
		r.detach(r.ctx.Err())
		return false
	}
}

// settle records what the resumed or relocated execution produced as the
// exchange's reply.
func (r *exchangeRecovery) settle(result turn.Result, err error) {
	r.stream.Phase(view.PhaseSaving)
	r.stream.Close()
	r.s.finish(r.e, r.s.resultReply(r.ctx, r.exchange, r.work, result, err), err)
}

// detach ends the worker without settling the exchange; the stream closes
// first so the page stops following before the exchange is handed back.
func (r *exchangeRecovery) detach(err error) {
	r.stream.Close()
	r.s.detachRecovery(r.e, err)
}

// settles says whether an execution's outcome closes the exchange: it
// answered, or it ran and failed for a reason other than being out of
// reach, which a fresh pass could not change.
func settles(result turn.Result, err error) bool {
	return err == nil || (result.Attempt != "" && !isRecoveryBlocked(err))
}

// blockedBy is the block to put to the owner: the recovery's own, or when
// the execution could not even be found, a generic one over both errors.
func blockedBy(err, lookupErr error) *turn.RecoveryBlocked {
	var blocked *turn.RecoveryBlocked
	if errors.As(err, &blocked) {
		return blocked
	}
	return blockedRecovery(errors.Join(lookupErr, err), "原执行需要核实", "已检查这条会话的执行记录。\n\n暂时找不到可以安全接回的原执行或完整结果。\n\n原任务可能仍在节点上运行，重新发送任务可能造成重复操作。\n\n建议检查原机器和执行记录，确认后重新检查；也可以保留任务等待处理。", "重新检查原执行")
}

// blockedRecovery is a block whose question offers the owner a retry, by
// the given label, or to wait.
func blockedRecovery(cause error, title, message, retryLabel string) *turn.RecoveryBlocked {
	return &turn.RecoveryBlocked{Cause: cause, Question: view.Question{Kind: "recovery", Title: title, Message: message, Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: retryLabel}, {Value: "wait", Label: "暂时等待", Detail: "保留任务和进度。Steve 会继续自己重连，原节点回来后自动接着跑。"}}}}
}

func isRecoveryBlocked(err error) bool {
	var blocked *turn.RecoveryBlocked
	return errors.As(err, &blocked)
}

func (s *Service) detachRecovery(e *queuedExchange, err error) {
	s.mu.Lock()
	if e.RecoveryStop != nil {
		reply := *e.RecoveryStop
		s.mu.Unlock()
		s.finish(e, reply, nil)
		return
	}
	defer s.mu.Unlock()
	s.detachRecoveryLocked(e, err)
}
func (s *Service) detachRecoveryLocked(e *queuedExchange, err error) {
	delete(s.processes, e.ID)
	if s.running[e.Conversation] > 0 {
		s.running[e.Conversation]--
	}
	e.outcome = outcome{err: err}
	e.ctx, e.cancel = nil, nil
	select {
	case <-e.done:
	default:
		close(e.done)
	}
}

func (s *Service) resultReply(ctx context.Context, exchange Exchange, work *process, result turn.Result, err error) consoleapi.Reply {
	reply := consoleapi.Reply{At: time.Now().UTC(), Conversation: exchange.Conversation, ProjectID: exchange.ExpectedProject, Title: result.Title, Text: result.Text, Kind: "reply", Process: work.summary(), Refs: exchange.Refs, Materials: exchange.Materials}
	if result.Attempt != "" && s.inspector != nil {
		if changes, cerr := s.inspector.Changes(ctx, result.Attempt); cerr != nil {
			slog.Error(fmt.Sprintf("console: changes of attempt %s: %v", result.Attempt, cerr), "attempt", result.Attempt, "conversation", exchange.Conversation, "exchange", exchange.ID)
		} else if changes != nil {
			reply.Changes = changes
		}
	}
	if in := result.Injected; in != nil {
		reply.Injected = &consoleapi.Injected{Project: in.Project, Workspace: in.Workspace, Agent: in.Agent, Node: in.Node, Harness: in.Harness, Model: in.Model, Options: in.Options, Session: in.Session, NewSession: in.NewSession, InstructionsSent: in.InstructionsSent, Instructions: in.Instructions, InstructionsBytes: in.InstructionsBytes, MCPServers: in.MCPServers, Fingerprint: in.Fingerprint, Prompt: in.Prompt, Sections: sectionsOf(in.Sections)}
	}
	if err != nil {
		reply.Error = err.Error()
		if reply.Text == "" {
			reply.Text = err.Error()
		}
	}
	return reply
}

// sectionsOf carries the instruction breakdown to the page.
func sectionsOf(in []capability.Section) []consoleapi.Section {
	out := make([]consoleapi.Section, 0, len(in))
	for _, s := range in {
		out = append(out, consoleapi.Section{Kind: s.Kind, Name: s.Name, Path: s.Path, Bytes: s.Bytes})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
