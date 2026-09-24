package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ErrExecuting is work that cannot be deleted yet: an attempt of it is
// still open, so the caller stops it first.
var ErrExecuting = errors.New("task is executing")

// DeleteChannel removes what one conversation opened: the tasks it holds
// and everything delegated from them, with their organization. A task is
// the record of work asked for in a thread, so it goes when the thread
// goes; leaving it behind would list work nobody can open any more.
//
// Work in flight is never deleted out from under itself: a task with an
// open execution refuses, and the caller stops it first.
func (s *Store) DeleteChannel(channel string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.deletableLocked(channel)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return ids, nil
	}
	next := s.draft()
	for _, id := range ids {
		next.remove(id)
	}
	if err := s.replaceLocked(next); err != nil {
		return nil, err
	}
	return ids, nil
}

// ChannelIdle reports what a conversation opened as safe to delete: no
// task of it, or delegated from it, has an attempt open. The caller asks
// before ending anything else, so a refusal costs the owner nothing.
func (s *Store) ChannelIdle(channel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.deletableLocked(channel)
	return err
}

// deletableLocked is the conversation's tasks and everything delegated
// from them, in order, or ErrExecuting for the first one still running.
func (s *Store) deletableLocked(channel string) ([]string, error) {
	doomed := map[string]bool{}
	for id, stored := range s.data.Tasks {
		if stored.Channel == channel {
			doomed[id] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for id, stored := range s.data.Tasks {
			if doomed[id] || stored.Parent == "" || !doomed[stored.Parent] {
				continue
			}
			doomed[id] = true
			changed = true
		}
	}
	ids := make([]string, 0, len(doomed))
	for id := range doomed {
		if s.data.Tasks[id].HasOpenExecution() {
			return nil, fmt.Errorf("%w: %s", ErrExecuting, id)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return lessID(ids[i], ids[j]) })
	return ids, nil
}

// DeletedTx proves under the caller's snapshot that a task was deleted: the
// store issued its id, and neither its header, its organization nor any of
// its accounting rows remain. Deleting a conversation removes exactly these
// together, so a record that still names the task has nothing left to
// account or deliver. A missing header alone is not proof; it may be a
// partial or damaged store, which stays an error for its callers.
func DeletedTx(tx ledger.Reader, id string) (bool, error) {
	n, err := strconv.Atoi(id)
	if err != nil || n < 1 || strconv.Itoa(n) != id {
		return false, nil
	}
	var control recordControl
	found, err := readRecordTx(tx, taskStoreKind, taskStoreID, &control)
	if err != nil || !found || n >= control.NextID {
		return false, err
	}
	var raw json.RawMessage
	for _, kind := range []string{taskKind, taskMetaKind} {
		if found, err := readRecordTx(tx, kind, id, &raw); err != nil || found {
			return false, err
		}
	}
	prefix := attemptPrefix(id)
	var rows int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM bindings WHERE kind=? AND substr(id,1,?)=?`, taskAttemptKind, len(prefix), prefix).Scan(&rows); err != nil {
		return false, err
	}
	return rows == 0, nil
}
