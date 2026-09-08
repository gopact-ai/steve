package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// NodeSessionContext must come from the committed task/attempt and input
// records. Reattachment uses the same IDs; a new process cannot mint substitutes.
type NodeSessionContext struct {
	Authority     nodewire.SessionAuthority
	Binding       nodewire.SessionBinding
	CommandID     string
	InputSequence uint64
}

type nodeSessionContextKey struct{}

func WithNodeSession(ctx context.Context, binding NodeSessionContext) context.Context {
	return context.WithValue(ctx, nodeSessionContextKey{}, binding)
}

func NodeSessionFromContext(ctx context.Context) (NodeSessionContext, bool) {
	binding, ok := ctx.Value(nodeSessionContextKey{}).(NodeSessionContext)
	return binding, ok
}

type nativeQuestionKey struct{}
type nativeQuestionSource struct {
	Binding   nodewire.SessionBinding
	SessionID string
	RequestID string
}

// NativeQuestionSource exists only inside a managed node question callback.
// Consumers use it to bind their pending UI record to the actual node request.
func NativeQuestionSource(ctx context.Context) (nodewire.SessionBinding, string, string, bool) {
	source, ok := ctx.Value(nativeQuestionKey{}).(nativeQuestionSource)
	return source.Binding, source.SessionID, source.RequestID, ok
}

// NodeSessionTransport performs authenticated node RPC. Capability support is
// checked before use; known managed-session IDs never fall back to raw ACP.
type NodeSessionTransport interface {
	NodeSession(context.Context, string, nodewire.SessionRequest) (nodewire.SessionState, error)
}

var ErrNodeSessionUnavailable = errors.New("node-owned session unavailable; original execution must be reconciled")

// NodeSessionOpenUncertain carries the committed identity of an attempted
// node-owned open even when its response did not supply a native session ID.
// Consumers can detach this observer; they cannot infer stop or replay safety.
type NodeSessionOpenUncertain struct {
	Binding       nodewire.SessionBinding
	OpenCommandID string
	SessionID     string
	Cause         error
}

func (e *NodeSessionOpenUncertain) Error() string {
	return fmt.Sprintf("node-owned open for attempt %s on %s has no confirmed response: %v", e.Binding.AttemptID, e.Binding.NodeID, e.Cause)
}
func (e *NodeSessionOpenUncertain) Unwrap() error {
	return errors.Join(ErrNodeSessionUnavailable, ErrStopUnconfirmed, e.Cause)
}

type managedSession struct {
	at        Placement
	transport NodeSessionTransport
	base      NodeSessionContext
	id        string
	mu        sync.Mutex
	state     nodewire.SessionState
	observe   Observer
	answers   map[string]nodewire.SessionAnswer
	stopDone  chan struct{}
	stopErr   error
	stopState nodewire.SessionState
}

func (m *Manager) openNodeSession(ctx context.Context, at Placement, upstreamID, workdir string, servers []acp.MCPServer) (Runner, bool, error) {
	binding, bound := NodeSessionFromContext(ctx)
	managed := strings.HasPrefix(upstreamID, "ns_")
	if !bound && !managed {
		return nil, false, nil
	}
	if !bound {
		return nil, true, fmt.Errorf("%w: committed execution binding is required", ErrNodeSessionUnavailable)
	}
	if upstreamID != "" && !managed {
		return nil, true, fmt.Errorf("%w: old ACP session cannot be converted into a node-owned execution", ErrNodeSessionUnavailable)
	}
	node := at.Node
	if node == "" {
		node = binding.Binding.NodeID
	}
	if node == "" || binding.Binding.NodeID != node {
		return nil, true, fmt.Errorf("%w: execution placement differs", ErrNodeSessionUnavailable)
	}
	m.mu.Lock()
	transport, ok := m.remote.(NodeSessionTransport)
	observe, stopped := m.observe, m.stopped
	policy := m.configs[at.Harness].Permission
	m.mu.Unlock()
	if !ok || stopped {
		return nil, true, ErrNodeSessionUnavailable
	}
	if policy == "" {
		policy = permission.PolicyRead
	}
	request := nodewire.SessionRequest{Action: nodewire.SessionActionOpen, Authority: binding.Authority, Binding: binding.Binding, ID: upstreamID, Harness: at.Harness, Workdir: workdir, MCPServers: servers, Permission: policy, CommandID: binding.CommandID + "/open"}
	state, err := transport.NodeSession(ctx, node, request)
	if err != nil {
		var notSent *nodewire.SessionNotDispatched
		if upstreamID == "" && errors.As(err, &notSent) {
			return nil, true, fmt.Errorf("node-owned session was not opened: %w", err)
		}
		return nil, true, &NodeSessionOpenUncertain{Binding: binding.Binding, OpenCommandID: request.CommandID, SessionID: upstreamID, Cause: err}
	}
	for state.State == nodewire.SessionOpening {
		poll := request
		poll.Action = nodewire.SessionActionPoll
		poll.ID = state.ID
		poll.After = state.Sequence
		poll.WaitMS = 1000
		state, err = transport.NodeSession(ctx, node, poll)
		if err != nil {
			return nil, true, &NodeSessionOpenUncertain{Binding: binding.Binding, OpenCommandID: request.CommandID, SessionID: poll.ID, Cause: err}
		}
	}
	if state.State.Unavailable() {
		return nil, true, fmt.Errorf("%w: native session is %s", ErrNodeSessionUnavailable, state.State)
	}
	session := &managedSession{at: Placement{Node: node, Harness: at.Harness}, transport: transport, base: binding, id: state.ID, state: state, observe: observe, answers: map[string]nodewire.SessionAnswer{}}
	if err := m.registerSessionStop(ctx, session); err != nil {
		return nil, true, err
	}
	if observe != nil {
		observe(session.at, session.Settings())
	}
	return session, true, nil
}

func (s *managedSession) ID() string { return s.id }
func (s *managedSession) current(ctx context.Context) NodeSessionContext {
	if current, ok := NodeSessionFromContext(ctx); ok {
		return current
	}
	return s.base
}
func (s *managedSession) request(ctx context.Context, action nodewire.SessionAction) nodewire.SessionRequest {
	current := s.current(ctx)
	return nodewire.SessionRequest{Action: action, ID: s.id, Authority: current.Authority, Binding: current.Binding, CommandID: current.CommandID, InputSequence: current.InputSequence}
}
func (s *managedSession) call(ctx context.Context, request nodewire.SessionRequest) (nodewire.SessionState, error) {
	state, err := s.transport.NodeSession(ctx, s.at.Node, request)
	if err == nil {
		s.mu.Lock()
		if state.Sequence >= s.state.Sequence {
			s.state = state
		}
		s.mu.Unlock()
	}
	return state, err
}
func (s *managedSession) Settings() view.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.Settings
	out.Node = s.at.Node
	out.Harness = s.at.Harness
	return out
}
func (s *managedSession) ModelChoices() (string, []view.Choice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.ModelOption, append([]view.Choice(nil), s.state.ModelChoices...)
}
func (s *managedSession) Reobserve() {
	if s.observe != nil {
		s.observe(s.at, s.Settings())
	}
}
func (s *managedSession) SetModel(ctx context.Context, id, value string) error {
	return s.SetOption(ctx, id, value)
}
func (s *managedSession) SetOption(ctx context.Context, id, value string) error {
	request := s.request(ctx, nodewire.SessionActionOption)
	request.OptionID = id
	request.OptionValue = value
	_, err := s.call(ctx, request)
	return err
}
func (s *managedSession) Cancel(ctx context.Context) error {
	_, err := s.call(ctx, s.request(ctx, nodewire.SessionActionCancel))
	return err
}
func (s *managedSession) Abort() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.call(ctx, s.request(ctx, nodewire.SessionActionAbort)); err != nil {
		slog.Error(fmt.Sprintf("harness: abort node session on %s: %v", s.at.Node, err), "node", s.at.Node)
	}
}
func (s *managedSession) Stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.ProcessStopped
}
func (s *managedSession) Prompt(ctx context.Context, text string, progress func(view.Progress)) (string, []string, error) {
	return s.PromptTurn(ctx, text, nil, nil, nil, progress)
}

type managedPromptError struct{ message string }

func (e managedPromptError) Error() string       { return e.message }
func (e managedPromptError) PromptSettled() bool { return true }

func (s *managedSession) PromptTurn(ctx context.Context, text string, media []Media, ask permission.AskFunc, askUser acphost.AskUserFunc, progress func(view.Progress)) (output string, activity []string, runErr error) {
	request := s.request(ctx, nodewire.SessionActionPrompt)
	defer s.reconcileStop(request, &output, &activity, &runErr)
	s.mu.Lock()
	stopping := s.stopDone != nil
	s.mu.Unlock()
	if stopping {
		return "", nil, fmt.Errorf("%w: task stop already requested", ErrStopUnconfirmed)
	}
	request.Text = text
	for _, item := range media {
		request.Media = append(request.Media, nodewire.SessionMedia{MIME: item.MIME, Data: item.Data, URI: item.URI})
	}
	if request.InputSequence == 0 {
		attached, err := s.call(ctx, s.request(ctx, nodewire.SessionActionAttach))
		if err != nil {
			return "", nil, fmt.Errorf("%w: %w", ErrStopUnconfirmed, err)
		}
		if attached.Command != nil && attached.Command.ID == request.CommandID {
			request.InputSequence = attached.Command.InputSequence
		} else {
			request.InputSequence = attached.InputAccepted + 1
		}
	}
	state, err := s.call(ctx, request)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrStopUnconfirmed, err)
	}
	return s.follow(ctx, request, state, ask, askUser, progress)
}

// ResumableRunner observes an already accepted node command without resending
// or reconstructing its original prompt.
type ResumableRunner interface {
	Runner
	ResumeTurn(context.Context, permission.AskFunc, acphost.AskUserFunc, func(view.Progress)) (string, []string, error)
}

type RetainedSessionInspector interface {
	InspectRetained(context.Context) (nodewire.SessionState, error)
}

func (s *managedSession) InspectRetained(ctx context.Context) (nodewire.SessionState, error) {
	return s.call(ctx, s.request(ctx, nodewire.SessionActionAttach))
}

func (s *managedSession) ResumeTurn(ctx context.Context, ask permission.AskFunc, askUser acphost.AskUserFunc, progress func(view.Progress)) (output string, activity []string, runErr error) {
	request := s.request(ctx, nodewire.SessionActionAttach)
	defer s.reconcileStop(request, &output, &activity, &runErr)
	state, err := s.call(ctx, request)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrStopUnconfirmed, err)
	}
	if state.Command == nil || state.Command.ID != request.CommandID {
		return "", nil, fmt.Errorf("%w: accepted input receipt is unavailable", ErrStopUnconfirmed)
	}
	return s.follow(ctx, request, state, ask, askUser, progress)
}

func (s *managedSession) follow(ctx context.Context, request nodewire.SessionRequest, state nodewire.SessionState, ask permission.AskFunc, askUser acphost.AskUserFunc, progress func(view.Progress)) (string, []string, error) {
	var err error
	var delivered uint64
observe:
	for {
		if state.Sequence > delivered {
			if progress != nil {
				p := state.Progress
				p.Settings.Harness = s.at.Harness
				p.Settings.Node = s.at.Node
				progress(p)
			}
			delivered = state.Sequence
		}
		if state.Command != nil && state.Command.ID == request.CommandID {
			command := state.Command
			switch command.State {
			case nodewire.SessionCommandCompleted:
				if command.Error != "" {
					return command.Output, command.Activity, managedPromptError{command.Error}
				}
				return command.Output, command.Activity, nil
			case nodewire.SessionCommandCancelled:
				return command.Output, command.Activity, ErrTurnCanceled
			case nodewire.SessionCommandUncertain:
				return command.Output, command.Activity, fmt.Errorf("%w: %s", ErrStopUnconfirmed, command.Error)
			}
		}
		for _, q := range state.Questions {
			if q.State != "pending" || q.CommandID != request.CommandID {
				continue
			}
			s.mu.Lock()
			answer, known := s.answers[q.ID]
			s.mu.Unlock()
			if !known {
				var changed bool
				answer, changed, err = s.collectAnswer(ctx, request, q, ask, askUser)
				if err != nil {
					return "", nil, err
				}
				if changed {
					state, err = s.call(ctx, s.request(ctx, nodewire.SessionActionAttach))
					if err != nil {
						return "", nil, fmt.Errorf("%w: %w", ErrStopUnconfirmed, err)
					}
					continue observe
				}
				if answer.CommandID == "" {
					continue
				}
				s.mu.Lock()
				s.answers[q.ID] = answer
				s.mu.Unlock()
			}
			response := request
			response.Action = nodewire.SessionActionAnswer
			response.QuestionID = q.ID
			response.Answer = &answer
			response.Text = ""
			response.Media = nil
			if _, err := s.call(ctx, response); err != nil {
				return "", nil, fmt.Errorf("%w: answer receipt is uncertain: %w", ErrStopUnconfirmed, err)
			}
		}
		poll := request
		poll.Action = nodewire.SessionActionPoll
		poll.Text = ""
		poll.Media = nil
		poll.After = state.Sequence
		poll.WaitMS = 20000
		state, err = s.call(ctx, poll)
		if err != nil {
			return "", nil, fmt.Errorf("%w: observer detached: %w", ErrStopUnconfirmed, err)
		}
	}
}

// SetNodeSessionBinder attaches committed execution identities at the runtime
// boundary. Read-only probes may leave the context unchanged.
func (m *Manager) SetNodeSessionBinder(bind func(context.Context, Placement, string, string) (context.Context, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodeSessionBinder = bind
}
func (m *Manager) bindNodeSession(ctx context.Context, at Placement, upstream, workdir string) (context.Context, error) {
	m.mu.Lock()
	bind := m.nodeSessionBinder
	m.mu.Unlock()
	if bind == nil {
		return ctx, nil
	}
	return bind(ctx, at, upstream, workdir)
}

// AttachRetainedSession attaches only to an existing admitted execution. It
// never changes the native session configuration or binds a new attempt.
func (m *Manager) AttachRetainedSession(ctx context.Context, at Placement, upstreamID, workdir string) (ResumableRunner, error) {
	ctx, err := m.bindNodeSession(ctx, at, upstreamID, workdir)
	if err != nil {
		return nil, err
	}
	binding, bound := NodeSessionFromContext(ctx)
	if !bound || !strings.HasPrefix(upstreamID, "ns_") {
		return nil, ErrNodeSessionUnavailable
	}
	node := at.Node
	if node == "" {
		node = binding.Binding.NodeID
	}
	if node == "" || binding.Binding.NodeID != node {
		return nil, ErrNodeSessionUnavailable
	}
	m.mu.Lock()
	transport, ok := m.remote.(NodeSessionTransport)
	observe, stopped := m.observe, m.stopped
	m.mu.Unlock()
	if !ok || stopped {
		return nil, ErrNodeSessionUnavailable
	}
	state, err := transport.NodeSession(ctx, node, nodewire.SessionRequest{Action: nodewire.SessionActionAttach, ID: upstreamID, Authority: binding.Authority, Binding: binding.Binding, CommandID: binding.CommandID})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNodeSessionUnavailable, err)
	}
	session := &managedSession{at: Placement{Node: node, Harness: at.Harness}, transport: transport, base: binding, id: upstreamID, state: state, observe: observe, answers: map[string]nodewire.SessionAnswer{}}
	if err := m.registerSessionStop(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *managedSession) collectAnswer(ctx context.Context, request nodewire.SessionRequest, q nodewire.SessionQuestion, ask permission.AskFunc, askUser acphost.AskUserFunc) (nodewire.SessionAnswer, bool, error) {
	if (q.Permission != nil && ask == nil) || (q.Permission == nil && askUser == nil) {
		return nodewire.SessionAnswer{}, false, nil
	}
	questionCtx, cancelQuestion := context.WithCancel(ctx)
	questionCtx = WithNodeSession(questionCtx, s.current(ctx))
	questionCtx = context.WithValue(questionCtx, nativeQuestionKey{}, nativeQuestionSource{Binding: request.Binding, SessionID: s.id, RequestID: q.ID})
	watcherCtx, stopWatcher := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		var after uint64
		for {
			poll := request
			poll.Action = nodewire.SessionActionPoll
			poll.Text = ""
			poll.Media = nil
			poll.After = after
			poll.WaitMS = 1000
			state, err := s.call(watcherCtx, poll)
			if err != nil {
				cancelQuestion()
				done <- err
				return
			}
			pending := false
			for _, question := range state.Questions {
				if question.ID == q.ID && question.State == "pending" {
					pending = true
					break
				}
			}
			if !pending || state.Command == nil || state.Command.ID != request.CommandID || !state.Command.State.Active() {
				cancelQuestion()
				done <- nil
				return
			}
			after = state.Sequence
		}
	}()
	answer := nodewire.SessionAnswer{CommandID: "answer/" + q.ID}
	var callbackErr error
	if q.Permission != nil {
		nativeAsk := *q.Permission
		nativeAsk.SessionID = s.id
		nativeAsk.RequestID = q.ID
		outcome, err := ask(questionCtx, nativeAsk)
		callbackErr = err
		if outcome.Outcome == acp.RequestPermissionOutcomeTypeSelected {
			answer.Decision = "accept"
			answer.Choice = string(outcome.OptionID)
		} else {
			answer.Decision = "cancel"
		}
	} else {
		question := q.Question
		question.SessionID = s.id
		reply, err := askUser(questionCtx, question)
		callbackErr = err
		switch {
		case reply.Chosen():
			answer.Decision = "accept"
			answer.Choice = reply.Value
			answer.Text = reply.Text
		case reply.Decision == "decline":
			answer.Decision = "decline"
		case reply.Decision == "cancel":
			answer.Decision = "cancel"
		default:
			answer.CommandID = ""
		}
	}
	wasCancelled := questionCtx.Err() != nil
	stopWatcher()
	cancelQuestion()
	watchErr := <-done
	if ctx.Err() != nil {
		return nodewire.SessionAnswer{}, false, fmt.Errorf("%w: %w", ErrStopUnconfirmed, ctx.Err())
	}
	if wasCancelled {
		if watchErr == nil {
			return nodewire.SessionAnswer{}, true, nil
		}
		return nodewire.SessionAnswer{}, false, fmt.Errorf("%w: question observer detached: %w", ErrStopUnconfirmed, watchErr)
	}
	if callbackErr != nil {
		return nodewire.SessionAnswer{}, false, fmt.Errorf("%w: question callback detached: %w", ErrStopUnconfirmed, callbackErr)
	}
	if answer.CommandID == "" {
		return nodewire.SessionAnswer{}, false, fmt.Errorf("%w: question remains unanswered on the node", ErrStopUnconfirmed)
	}
	return answer, false, nil
}
