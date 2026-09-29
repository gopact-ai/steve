package node

import (
	"log/slog"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
)

// sessionProcess is what a record keeps of the native process group its open
// started, and where it ran: enough for a later node process to end the group
// and to prove the stop.
//
// The record says ProcessStopped=false before the agent starts and names its
// group only once it runs, so a node killed in between leaves a record that
// claims a process and names none. Such a stop stays unconfirmed; it is never
// taken for the absence of a process.
type sessionProcess struct {
	procgroup.Identity
	procgroup.Place
}

// recordedGroupWithin bounds how long ending recorded groups waits for their
// members to die before a stop is answered as unconfirmed.
const recordedGroupWithin = 2 * time.Second

// observeProcess is the host's Started callback. The host calls it holding
// its own lock, which a commit may take, so the identity is committed from
// another goroutine; the open waits for that commit before it publishes the
// agent.
func (one *ownedSession) observeProcess(id procgroup.Identity) {
	one.recording.Add(1)
	go func() {
		defer one.recording.Done()
		one.mu.Lock()
		defer one.mu.Unlock()
		next := one.copyLocked()
		next.Process = sessionProcess{Identity: id, Place: one.service.place}
		if err := one.commitLocked(next); err != nil {
			slog.Error("steve-node: native process group not recorded", "session", next.State.ID, "error", err)
		}
	}()
}

// endRecordedGroups ends at once, by one deadline, the process groups that
// the records an earlier node process left name, so a node that left many
// starts no later than one that left one. A stop left unconfirmed is tried
// again when a stop is asked for.
func (s *SessionService) endRecordedGroups(unstopped []*ownedSession) error {
	deadline := time.Now().Add(recordedGroupWithin)
	failed := make([]error, len(unstopped))
	var wg sync.WaitGroup
	for i, one := range unstopped {
		wg.Go(func() { failed[i] = s.endRecordedGroup(one, deadline) })
	}
	wg.Wait()
	for i, one := range unstopped {
		if failed[i] != nil {
			slog.Warn("steve-node: native process stop is not confirmed after restart", "session", one.record.State.ID, "error", failed[i])
			s.unverifiedProcesses[one.record.State.ID] = true
			continue
		}
		if err := s.endStoppedRuntime(one.record); err != nil {
			return err
		}
	}
	return nil
}

// endRecordedGroup ends the process group one's record names, left by an
// earlier node process, and records the stop once no member of it runs by
// deadline. one is not shared: its record was just read.
func (s *SessionService) endRecordedGroup(one *ownedSession, deadline time.Time) error {
	if !s.placeKnown {
		return procgroup.ErrUnsupported
	}
	process := one.record.Process
	if err := s.settle(process.Identity, process.Place, s.place, time.Until(deadline)); err != nil {
		return err
	}
	next := one.copyLocked()
	next.State.ProcessStopped = true
	for id, command := range next.Commands {
		command.ProcessStopped = true
		next.Commands[id] = command
	}
	return one.commitLocked(next)
}

// settleUnverified tries again to end the process group of a record an
// earlier node process left unconfirmed, and reports whether its stop is now
// recorded.
func (s *SessionService) settleUnverified(id string) bool {
	s.mu.Lock()
	unverified := s.unverifiedProcesses[id]
	s.mu.Unlock()
	if !unverified {
		return false
	}
	s.settleMu.Lock()
	defer s.settleMu.Unlock()
	record, exists, err := s.readRecord(id)
	if err != nil || !exists {
		return false
	}
	one := &ownedSession{service: s, record: record, changed: make(chan struct{})}
	if !record.State.ProcessStopped {
		if err := s.endRecordedGroup(one, time.Now().Add(recordedGroupWithin)); err != nil {
			slog.Warn("steve-node: native process stop is still not confirmed", "session", id, "error", err)
			return false
		}
	}
	s.mu.Lock()
	delete(s.unverifiedProcesses, id)
	s.mu.Unlock()
	if err := s.endStoppedRuntime(one.record); err != nil {
		slog.Error("steve-node: plugin shutdown receipt failed", "session", id, "error", err)
	}
	return true
}
