package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/processrestart"
)

// restartWaitInterval is how often a waiting restart looks again for the
// idle moment it needs. Checking costs one sealing attempt, which fails
// immediately while anything is running.
const restartWaitInterval = 2 * time.Second

// waitingRestart is a restart request that has been accepted by the
// coordinator but not yet applied: it holds the service's place until the
// work that a restart would have interrupted has finished.
type waitingRestart struct {
	name string
	op   consoleapi.RestartOperation
	stop context.CancelFunc
	done chan struct{}
}

// waitingRequest answers for a restart that is still waiting. An empty id
// asks for whatever this service is waiting for.
func (s *Services) waitingRequest(name, id string) (consoleapi.RestartOperation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wait == nil || s.wait.name != name || id != "" && s.wait.op.CommandID != id {
		return consoleapi.RestartOperation{}, false
	}
	return s.wait.op, true
}

// recorded returns an outcome this coordinator wrote for a command whose
// service does not know it, such as a wait that was withdrawn.
func (s *Services) recorded(id string) (consoleapi.RestartOperation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.history.Operations[id]
	return op, ok
}

// supersedes reports whether a different command is already waiting on the
// same service. The newer request describes the same intent against a
// newer program, so it takes the place rather than being turned away.
func (s *Services) supersedes(name, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending == "" && s.wait != nil && s.wait.name == name && s.wait.op.CommandID != id
}

// restartWhenIdle keeps the request and starts watching for the moment the
// service can restart without cutting anything short.
func (s *Services) restartWhenIdle(name string, req consoleapi.RestartRequest) (consoleapi.RestartOperation, error) {
	if name == "hub" {
		if !processrestart.Supported() {
			return consoleapi.RestartOperation{}, serviceFailure("unsupported", "Service restart is not supported on this platform")
		}
	} else if s.admin.Nodes == nil {
		return consoleapi.RestartOperation{}, serviceFailure("not_found", "Node not found")
	}
	if s.supersedes(name, req.CommandID) {
		s.withdraw(true)
	}
	s.mu.Lock()
	if s.pending != "" || s.wait != nil {
		s.mu.Unlock()
		return consoleapi.RestartOperation{}, serviceFailure("busy", "Another service restart is in progress")
	}
	op := consoleapi.RestartOperation{CommandID: req.CommandID, State: nodewire.RestartStateDraining, Mode: consoleapi.RestartWhenIdle,
		Incarnation: s.history.Incarnation, RequestedAt: time.Now().UTC(), WaitingOn: consoleapi.RestartWaitPreparing}
	ctx, cancel := context.WithCancel(context.Background())
	w := &waitingRestart{name: name, op: op, stop: cancel, done: make(chan struct{})}
	s.wait = w
	s.mu.Unlock()
	go s.awaitIdle(ctx, w)
	return op, nil
}

// withdraw ends the current wait. A withdrawal the owner asked for is
// recorded so their console can still read what became of the command; one
// that is superseded by an immediate restart leaves no trace to collide
// with the request that replaces it.
func (s *Services) withdraw(record bool) consoleapi.RestartOperation {
	s.mu.Lock()
	w := s.wait
	s.mu.Unlock()
	if w == nil {
		return consoleapi.RestartOperation{}
	}
	w.stop()
	<-w.done
	s.mu.Lock()
	defer s.mu.Unlock()
	op := w.op
	if s.wait != w {
		// It was applied while the withdrawal was being prepared.
		return op
	}
	op.State, op.WaitingOn, op.CompletedAt = nodewire.RestartStateCancelled, "", time.Now().UTC()
	op.WaitingConversations = nil
	s.wait, s.pending = nil, ""
	if record {
		s.history.Operations[op.CommandID] = op
		if err := s.persist(); err != nil {
			op.Error = err.Error()
		}
	}
	return op
}

// withdrawn answers a cancellation for a command that is not waiting: it
// either already settled, or never existed here.
func (s *Services) withdrawn(ctx context.Context, name, id string) (consoleapi.RestartOperation, error) {
	if op, ok := s.recorded(id); ok && name == "hub" {
		return op, nil
	}
	op, err := s.RestartStatus(ctx, name, id)
	if err != nil {
		return consoleapi.RestartOperation{}, err
	}
	if !op.State.Terminal() {
		return consoleapi.RestartOperation{}, serviceFailure("conflict", "This restart has already been accepted and cannot be withdrawn")
	}
	return op, nil
}

// awaitIdle applies the waiting restart at the first moment the service
// has nothing running, and until then records what it is waiting for.
func (s *Services) awaitIdle(ctx context.Context, w *waitingRestart) {
	defer close(w.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		err := s.apply(ctx, w)
		if err == nil || ctx.Err() != nil {
			return
		}
		if !s.note(w, err) {
			return
		}
		timer.Reset(restartWaitInterval)
	}
}

// apply makes one attempt to restart the service now.
func (s *Services) apply(ctx context.Context, w *waitingRestart) error {
	if w.name != "hub" {
		return s.applyNode(ctx, w)
	}
	release, err := s.seal(ctx, "hub")
	if err != nil {
		return err
	}
	if err := s.preflight(); err != nil {
		release()
		return err
	}
	if err := s.accept(w, release); err != nil {
		release()
		return err
	}
	return nil
}

// accept turns the waiting request into an accepted one and stops the
// service, which its launcher restarts on the program now installed.
func (s *Services) accept(w *waitingRestart, release func()) error {
	s.mu.Lock()
	if s.wait != w {
		s.mu.Unlock()
		return errors.New("the waiting restart was withdrawn")
	}
	op := w.op
	op.State, op.WaitingOn, op.WaitingConversations = nodewire.RestartStateAccepted, "", nil
	op.Incarnation, op.PreviousIncarnation = s.history.Incarnation, s.history.Incarnation
	s.history.Operations[op.CommandID] = op
	s.history.Latest = op.CommandID
	if err := s.persist(); err != nil {
		delete(s.history.Operations, op.CommandID)
		s.mu.Unlock()
		return err
	}
	w.op, s.wait, s.pending = op, nil, op.CommandID
	s.release, s.dispatched = release, true
	stop := s.stop
	s.mu.Unlock()
	stop()
	return nil
}

// applyNode restarts a node once the node itself reports that nothing is
// running on it. The node owns the command from there on.
func (s *Services) applyNode(ctx context.Context, w *waitingRestart) error {
	query, cancel := context.WithTimeout(ctx, 5*time.Second)
	st, err := s.admin.Nodes.RestartStatus(query, w.name, "")
	cancel()
	if err != nil {
		return &consoleapi.ServiceError{Code: "unavailable", Reason: consoleapi.RestartWaitOffline, Message: err.Error()}
	}
	if !st.Supported {
		return serviceFailure("unsupported", "The node does not support service restart")
	}
	if st.ActiveStreams != 0 || st.Processes != 0 {
		return serviceBusy(consoleapi.RestartWaitNode, "The node is still running work")
	}
	release, err := s.seal(ctx, w.name)
	if err != nil {
		return err
	}
	if err := s.waitNodeIdle(ctx, w.name); err != nil {
		release()
		return err
	}
	status, err := s.admin.Nodes.Restart(ctx, w.name, w.op.CommandID)
	if err != nil {
		release()
		return nodeRestartError(err)
	}
	s.mu.Lock()
	if s.wait == w {
		s.wait = nil
	}
	w.op, s.pending = nodeOperation(status), w.name+"/"+w.op.CommandID
	s.mu.Unlock()
	go s.watchNode(w.name, status.CommandID, release)
	return nil
}

// note records why the restart has not applied yet and reports whether
// waiting can still resolve it. Work in progress and a machine that is
// away both pass; anything else is a failure waiting cannot fix.
func (s *Services) note(w *waitingRestart, cause error) bool {
	reason := ""
	var subjects []string
	var failure *consoleapi.ServiceError
	if errors.As(cause, &failure) {
		reason, subjects = failure.Reason, failure.Subjects
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wait != w {
		return false
	}
	if reason == "" {
		w.op.State, w.op.WaitingOn, w.op.WaitingConversations = nodewire.RestartStateFailed, "", nil
		w.op.Error, w.op.CompletedAt = cause.Error(), time.Now().UTC()
		s.history.Operations[w.op.CommandID] = w.op
		if err := s.persist(); err != nil {
			w.op.Error = errors.Join(cause, err).Error()
		}
		s.wait, s.pending = nil, ""
		return false
	}
	if w.op.WaitingOn != reason {
		// One line each time the answer changes, so an upgrade that never
		// finds its moment can be read back afterwards.
		slog.Info(fmt.Sprintf("steve: restart %s is waiting on %s: %v", w.op.CommandID, reason, cause))
	}
	w.op.WaitingOn, w.op.WaitingConversations = reason, subjects
	return true
}
