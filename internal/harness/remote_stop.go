package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// SetStopRegistrar connects explicit task Pause/Cancel to its native session.
// The registrar owns task scope admission; inspection contexts can be no-ops.
// Manager.Stop and ordinary context cancellation keep their detach semantics.
func (m *Manager) SetStopRegistrar(register func(context.Context, string, func(context.Context) error) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopRegistrar = register
}

func (m *Manager) registerSessionStop(ctx context.Context, session *managedSession) error {
	m.mu.Lock()
	register := m.stopRegistrar
	m.mu.Unlock()
	if register == nil {
		return nil
	}
	return register(ctx, session.id, session.stopExecution)
}

// RetainedStopper stops an already admitted native command without observing
// or replaying its prompt. The caller supplies the original revoked task
// identity and the current coordinator activation through the session binder.
type RetainedStopper interface {
	StopRetained(context.Context) (nodewire.SessionState, error)
}

func (s *managedSession) StopRetained(ctx context.Context) (nodewire.SessionState, error) {
	if err := s.stopExecution(ctx); err != nil {
		return nodewire.SessionState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopState, nil
}

func (s *managedSession) stopExecution(ctx context.Context) (stopErr error) {
	ctx, finishStop := context.WithTimeout(ctx, 15*time.Second)
	defer finishStop()
	s.mu.Lock()
	if done := s.stopDone; done != nil {
		s.mu.Unlock()
		select {
		case <-done:
			return s.stopErr
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrStopUnconfirmed, ctx.Err())
		}
	}
	s.stopDone = make(chan struct{})
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.stopErr = stopErr
		close(s.stopDone)
	}()
	request := s.request(ctx, nodewire.SessionActionCancel)
	cancelCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	state, cancelErr := s.call(cancelCtx, request)
	cancel()
	if cancelErr == nil && stopReceiptMatches(state, request) && (state.ProcessStopped || (state.Command != nil && state.Command.ID == request.CommandID && state.Command.Settled)) {
		s.mu.Lock()
		s.stopState = state
		s.mu.Unlock()
		return nil
	}
	// An idle session without this input receipt can still have an authorized
	// Prompt waiting to enter its lock. Closing the native process fences that
	// delayed request; absence of an accepted command alone is not stop proof.
	request.Action = nodewire.SessionActionAbort
	state, abortErr := s.call(ctx, request)
	if abortErr == nil && stopReceiptMatches(state, request) && state.ProcessStopped {
		s.mu.Lock()
		s.stopState = state
		s.mu.Unlock()
		return nil
	}
	return errors.Join(ErrStopUnconfirmed, cancelErr, abortErr, errors.New("native stop receipt does not confirm original execution ended"))
}

func stopReceiptMatches(state nodewire.SessionState, request nodewire.SessionRequest) bool {
	return state.ID == request.ID && state.Binding == request.Binding
}

// A stop receipt can arrive while an observation RPC is being cancelled.
// Only an actual native settlement or process-exit receipt can replace the
// observation error. Other driver cleanup errors never pass through here.
func (s *managedSession) reconcileStop(request nodewire.SessionRequest, output *string, activity *[]string, runErr *error) {
	if *runErr != nil && !errors.Is(*runErr, ErrStopUnconfirmed) {
		return
	}
	s.mu.Lock()
	done := s.stopDone
	s.mu.Unlock()
	if done == nil && *runErr == nil {
		return
	}
	if done != nil {
		<-done
	}
	s.mu.Lock()
	state, stopped := s.state, s.stopState
	s.mu.Unlock()
	if stopReceiptMatches(stopped, request) {
		state = stopped
	}
	if !stopReceiptMatches(state, request) {
		return
	}
	if command := state.Command; command != nil && command.ID == request.CommandID {
		if command.Settled {
			switch command.State {
			case nodewire.SessionCommandCompleted:
				*output, *activity, *runErr = command.Output, command.Activity, nil
				if command.Error != "" {
					*runErr = managedPromptError{command.Error}
				} else if done != nil && stopReceiptMatches(stopped, request) {
					*runErr = ErrTurnCanceled
				}
			case nodewire.SessionCommandCancelled:
				*output, *activity, *runErr = command.Output, command.Activity, ErrTurnCanceled
			}
			return
		}
		if state.ProcessStopped || command.ProcessStopped {
			*output, *activity, *runErr = command.Output, command.Activity, managedPromptError{"original native process terminated before completing its prompt"}
		}
		return
	}
	if stopReceiptMatches(stopped, request) && stopped.Command == nil && stopped.ProcessStopped {
		*runErr = ErrTurnCanceled
	}
}
