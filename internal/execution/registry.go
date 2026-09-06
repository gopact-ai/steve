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
	registry     *Registry
	key          Key
	token        *task.ExecutionToken
	ctx          context.Context
	cancel       context.CancelCauseFunc
	stopLifetime func() bool
	done         chan struct{}
	finish       sync.Once
	err          error
}

type scopeKey struct{}

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
	s.ctx = context.WithValue(ctx, scopeKey{}, s)
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
	if s, ok := ctx.Value(scopeKey{}).(*Scope); ok {
		return s.Token()
	}
	return nil
}

// Finish belongs to the driver after durable outcome/resource cleanup. An
// unresolved error is kept so later stop requests cannot claim quiescence.
func (s *Scope) Finish(unresolved error) {
	s.finish.Do(func() {
		s.registry.mu.Lock()
		defer s.registry.mu.Unlock()
		s.err = unresolved
		if unresolved == nil {
			delete(s.registry.entries, s)
		}
		s.stopLifetime()
		s.cancel(context.Canceled)
		close(s.done)
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
	return waiting.Wait(ctx)
}

// Stop only signals already authorized IDs; task.SetAside owns persistence.
func (r *Registry) Stop(ids []string, cause error) WaitSet {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var waiting WaitSet
	for s := range r.entries {
		if set[s.key.TaskID] {
			s.cancel(cause)
			waiting = append(waiting, s)
		}
	}
	return waiting
}
func (w WaitSet) Wait(ctx context.Context) error {
	var failures []error
	for _, s := range w {
		select {
		case <-s.done:
			if s.err != nil {
				failures = append(failures, fmt.Errorf("task %s: %w", s.key.TaskID, s.err))
			}
		case <-ctx.Done():
			return errors.Join(append(failures, ctx.Err())...)
		}
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
