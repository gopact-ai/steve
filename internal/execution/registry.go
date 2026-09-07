// Package execution owns running work independently of incoming requests.
package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gopact-ai/steve/internal/task"
)

type Key struct{ TaskID, InstanceID, AttemptID string }

type Registry struct {
	mu       sync.Mutex
	tasks    *task.Store
	lifetime context.Context
	entries  map[*Scope]struct{}
	closing  bool
}

func New(lifetime context.Context, tasks *task.Store) *Registry {
	return &Registry{tasks: tasks, lifetime: lifetime, entries: map[*Scope]struct{}{}}
}

type Scope struct {
	registry      *Registry
	key           Key
	token         *task.ExecutionToken
	ctx           context.Context
	cancel        context.CancelCauseFunc
	stopLifetime  func() bool
	done          chan struct{}
	finish        sync.Once
	err           error
	stopHandlers  map[string]*stopHandler
	stopRequested bool
	stopComplete  <-chan struct{}
}

type scopeKey struct{}
type probeKey struct{}

// WithProbeKey identifies a committed execution for read-only node inspection.
// It supplies no task token or running ownership; the node authorizes each RPC.
func WithProbeKey(ctx context.Context, key Key) context.Context {
	ctx = context.WithValue(ctx, scopeKey{}, (*Scope)(nil))
	return context.WithValue(ctx, probeKey{}, key)
}

// Detached retains trace values while work belongs to the service lifetime.
// It does not retain the parent prompt's cancellation or deadline.
func (r *Registry) Detached(values context.Context) context.Context {
	return detached{Context: r.lifetime, values: values}
}

type detached struct {
	context.Context
	values context.Context
}

func (c detached) Value(key any) any { return c.values.Value(key) }

func (r *Registry) Begin(parent context.Context, key Key) (*Scope, error) {
	return r.begin(parent, key, nil)
}

// BeginAccepted owns a previously admitted result, including a completed
// task's deferred landing. It must retain its original authorization epoch.
func (r *Registry) BeginAccepted(parent context.Context, key Key, token *task.ExecutionToken) (*Scope, error) {
	return r.begin(parent, key, token)
}
func (r *Registry) begin(parent context.Context, key Key, accepted *task.ExecutionToken) (*Scope, error) {
	inherited := Token(parent)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return nil, context.Canceled
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if err := r.lifetime.Err(); err != nil {
		return nil, err
	}
	var token *task.ExecutionToken
	if key.TaskID != "" {
		if r.tasks == nil {
			return nil, errors.New("execution tasks are not wired")
		}
		current, err := r.tasks.ExecutionToken(key.TaskID)
		if accepted != nil {
			current = *accepted
			err = r.tasks.CheckExecution(current)
			if current.TaskID != key.TaskID {
				err = task.ErrExecutionStopped
			}
		}
		if err != nil {
			return nil, err
		}
		// Nested attempts inherit their parent's authorization, never a
		// resumed task's new epoch while stale work is still unwinding.
		if old := inherited; old != nil && old.TaskID == key.TaskID {
			if current != *old {
				return nil, task.ErrExecutionStopped
			}
		}
		token = &current
	}
	ctx, cancel := context.WithCancelCause(parent)
	s := &Scope{registry: r, key: key, token: token, cancel: cancel, done: make(chan struct{})}
	s.ctx = context.WithValue(context.WithValue(ctx, probeKey{}, false), scopeKey{}, s)
	s.stopLifetime = context.AfterFunc(r.lifetime, func() { cancel(context.Cause(r.lifetime)) })
	r.entries[s] = struct{}{}
	return s, nil
}
func (s *Scope) Context() context.Context { return s.ctx }
func (s *Scope) Done() <-chan struct{}    { return s.done }
func (s *Scope) Token() *task.ExecutionToken {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	if s.token == nil {
		return nil
	}
	t := *s.token
	return &t
}
func Token(ctx context.Context) *task.ExecutionToken {
	if s, ok := ctx.Value(scopeKey{}).(*Scope); ok && s != nil {
		return s.Token()
	}
	return nil
}

// KeyOf returns a running scope's identity or an explicitly bound read-only
// probe identity. A key grants no execution authority; node RPCs independently
// validate the committed task, attempt and coordinator activation.
func KeyOf(ctx context.Context) (Key, bool) {
	s, ok := ctx.Value(scopeKey{}).(*Scope)
	if !ok || s == nil {
		key, ok := ctx.Value(probeKey{}).(Key)
		return key, ok
	}
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	return s.key, true
}

// Finish belongs to the driver after durable outcome/resource cleanup. An
// unresolved error is kept so later stop requests cannot claim quiescence.
func (s *Scope) Finish(unresolved error) {
	s.finish.Do(func() {
		s.registry.mu.Lock()
		defer s.registry.mu.Unlock()
		s.err = unresolved
		s.stopLifetime()
		s.cancel(context.Canceled)
		close(s.done)
		s.pruneFinishedLocked()
	})
}

type WaitSet []*Scope

// Shutdown closes admission and waits for owners to record their outcomes
// before the service closes its ledger, transports and checkpoint store.
// A timeout is not proof that an external writer has stopped.
func (r *Registry) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closing = true
	waiting := make(WaitSet, 0, len(r.entries))
	for s := range r.entries {
		s.cancel(context.Canceled)
		waiting = append(waiting, s)
	}
	r.mu.Unlock()
	return waiting.wait(ctx, r.lifetime.Err() != nil)
}

// Stop acts only on already authorized IDs; task.SetAside owns persistence.
// Native handlers run before observer cancellation. Service shutdown and
// lifetime cancellation do not invoke these task-specific stop handlers.
func (r *Registry) Stop(ids []string, cause error) WaitSet {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	r.mu.Lock()
	var waiting WaitSet
	var stopping []*Scope
	var handlers, starting []*stopHandler
	for s := range r.entries {
		if set[s.key.TaskID] {
			waiting = append(waiting, s)
			if !s.stopRequested {
				s.stopRequested = true
				stopping = append(stopping, s)
			}
			for _, h := range s.stopHandlers {
				handlers = append(handlers, h)
				if !h.started {
					h.started = true
					starting = append(starting, h)
				}
			}
		}
	}
	if len(stopping) == 0 {
		r.mu.Unlock()
		return waiting
	}
	complete := make(chan struct{})
	for _, s := range stopping {
		s.stopComplete = complete
	}
	finish := func() {
		for _, h := range handlers {
			<-h.done
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, s := range stopping {
			s.cancel(cause)
		}
		close(complete)
		for _, s := range stopping {
			s.pruneFinishedLocked()
		}
	}
	r.mu.Unlock()
	for _, h := range starting {
		go h.run()
	}
	if len(handlers) == 0 {
		finish()
	} else {
		go finish()
	}
	return waiting
}
func (w WaitSet) Wait(ctx context.Context) error {
	return w.wait(ctx, false)
}

func (w WaitSet) wait(ctx context.Context, detachedObservers bool) error {
	var failures []error
	for _, s := range w {
		select {
		case <-s.done:
		case <-ctx.Done():
			return errors.Join(append(failures, ctx.Err())...)
		}
		s.registry.mu.Lock()
		complete := s.stopComplete
		var handlers []*stopHandler
		for _, h := range s.stopHandlers {
			if h.started {
				handlers = append(handlers, h)
			}
		}
		s.registry.mu.Unlock()
		if complete != nil {
			select {
			case <-complete:
			case <-ctx.Done():
				return errors.Join(append(failures, ctx.Err())...)
			}
		}
		for _, h := range handlers {
			select {
			case <-h.done:
				if h.err != nil {
					failures = append(failures, fmt.Errorf("task %s stop %s: %w", s.key.TaskID, h.key, h.err))
				}
			case <-ctx.Done():
				return errors.Join(append(failures, ctx.Err())...)
			}
		}
		s.registry.mu.Lock()
		if s.err != nil && !(detachedObservers && s.retainedDetached()) {
			failures = append(failures, fmt.Errorf("task %s: %w", s.key.TaskID, s.err))
		}
		s.registry.mu.Unlock()
	}
	return errors.Join(failures...)
}

// SetAttempt attaches a chat scope to the attempt opened after admission.
func (s *Scope) SetAttempt(id string) {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	s.key.AttemptID = id
}

// Resolve forgets a finished, quarantined scope after durable stop evidence
// was accepted by the attempt service. It cannot finish a still-running owner.
func (r *Registry) Resolve(attemptID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for s := range r.entries {
		if s.key.AttemptID != attemptID {
			continue
		}
		select {
		case <-s.done:
			delete(r.entries, s)
		default:
		}
	}
}

// BindTask attaches an admitted chat turn once task creation is durable.
// It shares the registry lock with Stop and rechecks the persisted epoch.
func (s *Scope) BindTask(id string) error {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	if s.token != nil {
		if err := s.registry.tasks.CheckExecution(*s.token); err != nil {
			return err
		}
	}
	token, err := s.registry.tasks.ExecutionToken(id)
	if err != nil {
		return err
	}
	if s.token != nil && s.token.TaskID == id && *s.token != token {
		return task.ErrExecutionStopped
	}
	s.key.TaskID = id
	s.token = &token
	return nil
}
