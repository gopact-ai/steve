package sshconnect

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodebootstrap"
)

// AutoStartBackend is offered by a backend that holds the SSH links of the
// machines it enrolled: automatic start brings back the peer of such a
// machine when its process is gone.
type AutoStartBackend interface {
	RestartBackend
	// Watched lists the machines this node holds a link to that are still
	// cluster members.
	Watched(ctx context.Context) []string
	// Answers reports whether the machine answers the cluster now.
	Answers(ctx context.Context, nodeID string) bool
	// Reachable reports whether the SSH session to the machine is up.
	Reachable(ctx context.Context, nodeID string) bool
}

const (
	// autoStartEvery is how often automatic start looks at the machines,
	// the pace at which the cluster redials a member it lost.
	autoStartEvery = 15 * time.Second
	// autoStartAfter is how long a machine stays unanswering before its
	// peer is started: longer than the cluster's own redial and the link's
	// reconnect backoff, so a network blip heals by itself first.
	autoStartAfter = time.Minute
	// autoStartLimit is how many starts in a row automatic start makes for
	// a machine that does not stay online before it stops for it.
	autoStartLimit = 5
	// autoStartSettle is how long a machine must answer for its failed or
	// short-lived starts to be forgotten, as long as a peer's own limit on
	// restarting itself counts over.
	autoStartSettle = 10 * time.Minute
	// autoStartBackoff is the wait after the first start; it doubles with
	// every further one: 1, 2, 4 and 8 minutes.
	autoStartBackoff = time.Minute
	// autoStartRecheck is how long a machine whose peer was found running
	// is left before it is looked at again.
	autoStartRecheck = 5 * time.Minute
)

// How automatic start stands for a machine.
const (
	autoWatching    = "watching"
	autoWaiting     = "waiting"
	autoUnreachable = "unreachable"
	autoAttempting  = "attempting"
	autoRetrying    = "retrying"
	autoPeerRunning = "peer_running"
	autoStopped     = "stopped"
)

// AutoStartState is how automatic start stands for one machine. State is
// watching (the machine answers, or was just brought back), waiting (it
// stopped answering less than a minute ago), unreachable (its SSH session
// is down), attempting (a start is running), retrying (a start failed and
// another follows at NextAt), peer_running (its peer runs but does not
// answer; Reason says so and it is left alone) or stopped (LastError is
// why no start follows until the machine stays online or is restarted by
// hand). Attempts counts the starts since the machine last stayed online.
type AutoStartState struct {
	State        string    `json:"state"`
	Attempts     int       `json:"attempts"`
	Limit        int       `json:"limit"`
	LastError    string    `json:"last_error,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	LastAt       time.Time `json:"last_at,omitzero"`
	NextAt       time.Time `json:"next_at,omitzero"`
	OfflineSince time.Time `json:"offline_since,omitzero"`
}

// autoWatch is automatic start's memory of one machine.
type autoWatch struct {
	state       AutoStartState
	onlineSince time.Time
	stopped     bool
	// peerRunning is set while the last start found the peer running, so
	// finding it running again is not recorded again.
	peerRunning bool
	// cancel ends the start running for the machine; nil when none runs.
	cancel context.CancelFunc
}

// forget drops what failed or short-lived starts left behind.
func (w *autoWatch) forget() {
	w.stopped, w.peerRunning = false, false
	w.state.Attempts, w.state.LastError, w.state.Reason, w.state.NextAt = 0, "", "", time.Time{}
}

// AutoStart starts watching the machines the backend holds links to,
// every autoStartEvery until the service closes: a machine that has not
// answered the cluster for autoStartAfter, whose SSH session is up, gets
// its peer started where none runs. A peer that still runs is never
// stopped; it is shown and left to a manual restart. It does nothing where
// the backend has no such machines.
func (s *Service) AutoStart() {
	if _, ok := s.backend.(AutoStartBackend); !ok {
		return
	}
	s.mu.Lock()
	if s.closed || s.autoStarted {
		s.mu.Unlock()
		return
	}
	s.autoStarted = true
	s.autoRuns.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.autoRuns.Done()
		ticker := time.NewTicker(autoStartEvery)
		defer ticker.Stop()
		for {
			s.sweep()
			select {
			case <-s.autoCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// sweep looks at every watched machine once and starts the peers that are
// due. A machine no longer watched is forgotten and its start cut off.
func (s *Service) sweep() {
	backend, ok := s.backend.(AutoStartBackend)
	if !ok || s.autoCtx.Err() != nil {
		return
	}
	ctx, text := s.speak(s.autoCtx)
	watched := backend.Watched(ctx)
	type look struct{ answers, reachable bool }
	looks := make([]look, len(watched))
	var wg sync.WaitGroup
	for i, nodeID := range watched {
		wg.Go(func() { looks[i] = look{backend.Answers(ctx, nodeID), backend.Reachable(ctx, nodeID)} })
	}
	wg.Wait()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for nodeID, watch := range s.watches {
		if !slices.Contains(watched, nodeID) {
			if watch.cancel != nil {
				watch.cancel()
			}
			delete(s.watches, nodeID)
		}
	}
	if s.watches == nil {
		s.watches = map[string]*autoWatch{}
	}
	for i, nodeID := range watched {
		watch := s.watches[nodeID]
		if watch == nil {
			watch = &autoWatch{state: AutoStartState{State: autoWatching, Limit: autoStartLimit}}
			s.watches[nodeID] = watch
		}
		if s.due(text, watch, nodeID, looks[i].answers, looks[i].reachable, now) {
			s.attempt(ctx, backend, watch, nodeID, now)
		}
	}
}

// due brings a machine's watch up to what the sweep saw and reports
// whether its peer is to be started now. s.mu is held.
func (s *Service) due(text i18n.Catalog, watch *autoWatch, nodeID string, answers, reachable bool, now time.Time) bool {
	if watch.cancel != nil {
		return false
	}
	if answers {
		watch.state.OfflineSince, watch.peerRunning = time.Time{}, false
		if watch.onlineSince.IsZero() {
			watch.onlineSince = now
		}
		if now.Sub(watch.onlineSince) >= autoStartSettle {
			watch.forget()
		}
		if !watch.stopped {
			watch.state.State, watch.state.Reason = autoWatching, ""
		}
		return false
	}
	watch.onlineSince = time.Time{}
	if watch.state.OfflineSince.IsZero() {
		watch.state.OfflineSince = now
	}
	switch {
	case watch.stopped:
		return false
	case now.Sub(watch.state.OfflineSince) < autoStartAfter:
		watch.state.State = autoWaiting
		return false
	case watch.state.Attempts >= autoStartLimit:
		// Every start came back and died again before the machine stayed
		// online long enough to be trusted.
		watch.stopped = true
		watch.state.State, watch.state.NextAt = autoStopped, time.Time{}
		watch.state.LastError = text.T(i18n.SSHAutoStartGaveUp, autoStartLimit, int(autoStartSettle/time.Minute))
		return false
	case now.Before(watch.state.NextAt):
		return false
	case !reachable:
		watch.state.State = autoUnreachable
		return false
	case s.running(s.upgrades, nodeID) || s.running(s.restarts, nodeID):
		return false
	}
	return true
}

// attempt starts the peer of nodeID where none runs, in the background,
// as a restart any upgrade or manual restart of the machine waits for.
// s.mu is held.
func (s *Service) attempt(ctx context.Context, backend AutoStartBackend, watch *autoWatch, nodeID string, now time.Time) {
	stored, failure := s.claimRestart(i18n.FromContext(ctx), nodeID, true)
	if failure != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	watch.cancel = cancel
	watch.state.State, watch.state.LastAt = autoAttempting, now
	s.autoRuns.Add(1)
	go func() {
		defer s.autoRuns.Done()
		defer cancel()
		result, outcome, err := s.restart(ctx, backend, stored.plan.ID, nodeID, nodebootstrap.RestartSpec{IfStopped: true})
		if record, ok := s.concluded(ctx, watch, nodeID, outcome, err); ok {
			s.record(ctx, backend, &result, record)
		}
		s.settle(stored, result, err)
	}()
}

// concluded brings a machine's watch up to what its start did and returns
// the record to keep of it, if any. A start cut off because the machine
// left the cluster or the service closed leaves nothing behind.
func (s *Service) concluded(ctx context.Context, watch *autoWatch, nodeID, outcome string, err error) (RestartRecord, bool) {
	now := s.now()
	record := RestartRecord{NodeID: nodeID, Automatic: true, Outcome: outcome, Reason: reasonOf(err)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watches[nodeID] != watch {
		return record, false
	}
	watch.cancel = nil
	if ctx.Err() != nil {
		return record, false
	}
	var failure *StepError
	errors.As(err, &failure)
	running := outcome == RestartRunning
	recorded := !running || !watch.peerRunning
	watch.peerRunning = running
	switch {
	case err == nil:
		watch.state.Attempts++
		watch.state.State, watch.state.LastError, watch.state.Reason = autoWatching, "", ""
		watch.state.NextAt = now.Add(autoStartBackoff << (watch.state.Attempts - 1))
	case running:
		watch.state.State, watch.state.Reason = autoPeerRunning, record.Reason
		watch.state.NextAt = now.Add(autoStartRecheck)
	case failure != nil && failure.Stage == "ssh":
		// The machine could not be reached over SSH: nothing was tried on
		// it, so nothing counts or is recorded.
		watch.state.State, watch.state.LastError = autoUnreachable, record.Reason
		watch.state.NextAt = now.Add(autoStartBackoff)
		return record, false
	case failure != nil && failure.Code == "restart_busy":
		// Another installation holds the machine; its turn comes first.
		watch.state.State, watch.state.LastError = autoRetrying, record.Reason
		watch.state.NextAt = now.Add(autoStartBackoff)
	default:
		watch.state.Attempts++
		watch.state.LastError = record.Reason
		if watch.state.Attempts >= autoStartLimit {
			watch.stopped = true
			watch.state.State, watch.state.NextAt = autoStopped, time.Time{}
			record.Outcome = RestartStopped
		} else {
			watch.state.State = autoRetrying
			watch.state.NextAt = now.Add(autoStartBackoff << (watch.state.Attempts - 1))
		}
	}
	return record, recorded
}

// resumeAutoStart forgets a machine's failed starts once a manual restart
// brought it back. s.mu is held.
func (s *Service) resumeAutoStart(nodeID string) {
	if watch := s.watches[nodeID]; watch != nil {
		watch.forget()
		watch.state.State = autoWatching
	}
}
