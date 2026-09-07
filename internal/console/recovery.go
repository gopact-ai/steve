package console

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
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
			if (e.State != "recovering" && e.State != "awaiting-user") || e.cancel != nil {
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
	e.State = "recovering"
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

func (s *Service) recoverExchange(ctx context.Context, e *queuedExchange, driver RetainedChatDriver) {
	s.mu.Lock()
	exchange := copyExchange(e.Exchange)
	work := newProcess()
	if s.processes == nil {
		s.processes = map[string]*process{}
	}
	s.processes[e.ID] = work
	s.mu.Unlock()
	stop := s.follow(ctx, exchange.Conversation, work)
	defer stop()
	if s.anchor != nil {
		s.anchor(exchange.Conversation, ChatID, AnchorMark+exchange.ID)
	}
	quiet := false
	waitingPlan := ""
	for {
		if ctx.Err() != nil {
			s.detachRecovery(e, ctx.Err())
			return
		}
		candidate, found, lookupErr := s.findRetained(ctx, driver, exchange)
		base := consoleapi.PendingQuestion{Conversation: exchange.Conversation, ExchangeID: exchange.ID, Project: exchange.ExpectedProject, TaskID: candidate.TaskID, AttemptID: candidate.AttemptID, Locale: exchange.Locale}
		if found && candidate.ProjectID != "" {
			base.Project = candidate.ProjectID
		}
		requester := s.owner
		if exchange.Requester != "" {
			requester = exchange.Requester
		}
		if requester == "" || requester != s.owner {
			s.finish(e, consoleapi.Reply{}, errors.New("recovery requires the original console owner"))
			return
		}
		var result turn.Result
		var err error
		var identityMu sync.Mutex
		questionBase := func() consoleapi.PendingQuestion { identityMu.Lock(); defer identityMu.Unlock(); return base }
		request := turn.Request{Channel: "console", ConversationID: exchange.Conversation, MessageID: AnchorMark + exchange.ID, ChatID: ChatID, SenderOpenID: requester, ChatType: protocol.ChatP2P, Mentioned: true, Origin: exchange.Origin, ExpectedProject: exchange.ExpectedProject, Locale: exchange.Locale,
			OnTurnReady: func(taskID, attemptID string) {
				identityMu.Lock()
				base.TaskID, base.AttemptID = taskID, attemptID
				identityMu.Unlock()
			},
			OnProgress: s.progress(exchange.Conversation, exchange.ID, work),
			OnAsk: func(ctx context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
				return s.askPermission(ctx, questionBase(), ask)
			},
			OnAskUser: func(ctx context.Context, q view.Question) (view.Answer, error) {
				return s.askUser(ctx, questionBase(), q)
			},
		}
		if found && lookupErr == nil {
			s.mu.Lock()
			e.State = "recovering"
			saveErr := s.save()
			s.publishQueue(e.Conversation)
			s.mu.Unlock()
			if saveErr != nil {
				if ctx.Err() != nil {
					s.detachRecovery(e, ctx.Err())
					return
				}
				lookupErr = saveErr
			} else {
				if candidate.plan != nil {
					result, err = driver.(retainedPlanDriver).ResumeRetainedPlan(ctx, *candidate.plan, request)
				} else {
					result, err = driver.ResumeRetainedChat(ctx, candidate.AttemptID, request)
				}
				if err == nil || (result.Attempt != "" && !isRecoveryBlocked(err)) {
					s.finish(e, s.resultReply(ctx, exchange, work, result, err), err)
					return
				}
			}
		}
		if ctx.Err() != nil {
			s.detachRecovery(e, ctx.Err())
			return
		}
		if planner, supportsRelocation := driver.(relocationDriver); supportsRelocation && found && candidate.plan == nil && isRecoveryBlocked(err) {
			input, images, captureErr := s.relocationInput(ctx, exchange, candidate.ProjectID, requester)
			if captureErr == nil {
				request.Relocation, request.Images = input, images
				plan, planErr := planner.PlanRelocation(ctx, candidate.AttemptID, request)
				if planErr == nil && plan.ID != "" {
					signature := plan.ID + "/" + plan.Question.Message
					if quiet && waitingPlan != "" && signature != waitingPlan {
						quiet = false
					}
					if !quiet {
						choice := s.approvedRelocation(questionBase(), plan.ID)
						if !plan.Automatic && !plan.Approved && choice == "" {
							s.mu.Lock()
							e.State = "awaiting-user"
							saveErr := s.save()
							s.publishQueue(e.Conversation)
							s.mu.Unlock()
							if saveErr != nil {
								s.detachRecovery(e, saveErr)
								return
							}
							answer, askErr := s.RequestRecovery(ctx, questionBase(), plan.Question)
							if askErr != nil {
								s.detachRecovery(e, askErr)
								return
							}
							if answer.Value != "confirm-stopped-and-retry:"+plan.ID {
								quiet = true
								waitingPlan = signature
							} else {
								choice = answer.Value
							}
						}
						if !quiet {
							result, relocationErr := planner.RelocateChat(ctx, plan.ID, choice, request)
							if relocationErr == nil || (result.Attempt != "" && !isRecoveryBlocked(relocationErr)) {
								s.finish(e, s.resultReply(ctx, exchange, work, result, relocationErr), relocationErr)
								return
							}
							err = &turn.RecoveryBlocked{Cause: relocationErr, Question: view.Question{Kind: "recovery", Title: "恢复方案需要重新检查", Message: "已按确认的恢复方案检查执行条件。\n\n本次恢复尚未完成：" + relocationErr.Error() + "\n\n已保存的原任务和恢复记录仍保留，没有将失败当作完成。\n\n建议重新检查当前执行，或等待原节点恢复。", Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: "重新检查"}, {Value: "wait", Label: "暂时等待"}}}}
						}
					}
				} else if isRecoveryBlocked(planErr) {
					err = planErr
				}
			} else {
				err = &turn.RecoveryBlocked{Cause: captureErr, Question: view.Question{Kind: "recovery", Title: "恢复上下文需要处理", Message: "已读取原输入与冻结材料。\n\n当前无法完整重建恢复上下文：" + captureErr.Error() + "\n\n不会用新文件或猜测替换原材料。\n\n建议恢复原材料后重新检查。", Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: "重新检查"}, {Value: "wait", Label: "暂时等待"}}}}
			}
		}
		var blocked *turn.RecoveryBlocked
		if !errors.As(err, &blocked) {
			cause := errors.Join(lookupErr, err)
			blocked = &turn.RecoveryBlocked{Cause: cause, Question: view.Question{Kind: "recovery", Title: "原执行需要核实", Message: "已检查这条会话的执行记录。\n\n暂时找不到可以安全接回的原执行或完整结果。\n\n原任务可能仍在节点上运行，重新发送任务可能造成重复操作。\n\n建议检查原机器和执行记录，确认后重新检查；也可以保留任务等待处理。", Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: "重新检查原执行"}, {Value: "wait", Label: "暂时等待"}}}}
		}
		s.mu.Lock()
		e.State = "awaiting-user"
		saveErr := s.save()
		s.publishQueue(e.Conversation)
		s.mu.Unlock()
		if saveErr != nil {
			if ctx.Err() != nil {
				s.detachRecovery(e, ctx.Err())
				return
			}
			s.detachRecovery(e, saveErr)
			return
		}
		if quiet {
			timer := time.NewTimer(30 * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				s.detachRecovery(e, ctx.Err())
				return
			}
			continue
		}
		question := blocked.Question
		question.Kind = "recovery"
		answer, askErr := s.askUser(ctx, base, question)
		if askErr != nil {
			if ctx.Err() != nil {
				s.detachRecovery(e, ctx.Err())
				return
			}
			s.detachRecovery(e, askErr)
			return
		}
		// Free-form advice is retained in the question record. Without an
		// explicit retry choice, make one fresh observation and then wait
		// quietly instead of repeatedly asking the same unresolved question.
		quiet = answer.Value != "retry"
	}
}

func isRecoveryBlocked(err error) bool {
	var blocked *turn.RecoveryBlocked
	return errors.As(err, &blocked)
}

func (s *Service) detachRecovery(e *queuedExchange, err error) {
	s.mu.Lock()
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
			log.Printf("console: changes of attempt %s: %v", result.Attempt, cerr)
		} else if changes != nil {
			reply.Changes = changes
		}
	}
	if in := result.Injected; in != nil {
		reply.Injected = &consoleapi.Injected{Project: in.Project, Workspace: in.Workspace, Agent: in.Agent, Node: in.Node, Harness: in.Harness, Model: in.Model, Options: in.Options, Session: in.Session, NewSession: in.NewSession, InstructionsSent: in.InstructionsSent, Instructions: in.Instructions, InstructionsBytes: in.InstructionsBytes, MCPServers: in.MCPServers, Fingerprint: in.Fingerprint, Prompt: in.Prompt}
	}
	if err != nil {
		reply.Error = err.Error()
		if reply.Text == "" {
			reply.Text = err.Error()
		}
	}
	return reply
}
