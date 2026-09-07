package execution

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

const stopHandlerTimeout = 15 * time.Second

type stopHandler struct {
	scope   *Scope
	key     string
	values  context.Context
	stop    func(context.Context) error
	started bool
	done    chan struct{}
	err     error
}

// RegisterStopHandler binds native cleanup to an actual execution scope.
// Read-only probes have no scope and install nothing. A late native open that
// races Stop is stopped before registration returns ErrExecutionStopped, so
// the caller cannot continue with the newly returned native session.
func RegisterStopHandler(ctx context.Context, key string, stop func(context.Context) error) error {
	if _, probe := ctx.Value(probeKey{}).(Key); probe {
		return nil
	}
	s, ok := ctx.Value(scopeKey{}).(*Scope)
	if !ok || s == nil {
		return nil
	}
	if key == "" || stop == nil {
		return errors.New("native stop requires an identity and handler")
	}
	r := s.registry
	r.mu.Lock()
	select {
	case <-s.done:
		r.mu.Unlock()
		return errors.New("native stop cannot register after execution owner finished")
	default:
	}
	h, exists := s.stopHandlers[key]
	if !exists {
		h = &stopHandler{scope: s, key: key, values: context.WithoutCancel(ctx), stop: stop, done: make(chan struct{})}
		if s.stopHandlers == nil {
			s.stopHandlers = map[string]*stopHandler{}
		}
		s.stopHandlers[key] = h
	}
	if !s.stopRequested {
		r.mu.Unlock()
		return nil
	}
	start := !h.started
	h.started = true
	r.mu.Unlock()
	if start {
		go h.run()
	}
	<-h.done
	return errors.Join(task.ErrExecutionStopped, h.err)
}

func (h *stopHandler) run() {
	ctx, cancel := context.WithTimeout(h.values, stopHandlerTimeout)
	err := h.stop(ctx)
	cancel()
	r := h.scope.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	h.err = err
	close(h.done)
	h.scope.pruneFinishedLocked()
}

// Registry entries remain until both the driver and any explicit native stop
// have finished successfully. A timeout or driver completion supplies no
// missing native evidence and never removes a failed stop.
func (s *Scope) pruneFinishedLocked() {
	select {
	case <-s.done:
	default:
		return
	}
	if s.err != nil {
		return
	}
	if s.stopComplete != nil {
		select {
		case <-s.stopComplete:
		default:
			return
		}
	}
	for _, h := range s.stopHandlers {
		if !h.started {
			continue
		}
		select {
		case <-h.done:
			if h.err != nil {
				return
			}
		default:
			return
		}
	}
	delete(s.registry.entries, s)
}
