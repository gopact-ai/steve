// Package intent records every side effect an agent asks Steve to make on
// its behalf — today, the milestone messages it posts to the chat.
//
// An intent is claimed by exactly one attempt, dispatched with a journal
// entry before the call leaves, and confirmed with the receipt after. A
// call that may or may not have gone out is outcome-unknown, and stays so
// until a person says what happened. A later attempt of the same task
// that asks for the same call is blocked until then: nothing is sent
// twice on a guess.
package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

type State string

const (
	Claimed    State = "claimed"
	Dispatched State = "dispatched"
	Succeeded  State = "succeeded"
	Failed     State = "failed"
	Unknown    State = "outcome-unknown"
	kind             = "intent"
)

type Intent struct {
	RequiresReconciliation bool            `json:"requires_reconciliation,omitempty"`
	ID                     string          `json:"id"`
	TaskID                 string          `json:"task_id"`
	AttemptID              string          `json:"attempt_id"`
	Tool                   string          `json:"tool"`
	Fingerprint            string          `json:"fingerprint"`
	State                  State           `json:"state"`
	Receipt                json.RawMessage `json:"receipt,omitempty"`
	Error                  string          `json:"error,omitempty"`
	Resolution             string          `json:"resolution,omitempty"`
	At                     time.Time       `json:"at"`
}

// Blocked is a claim refused because the same task's identical call is
// still active or has an unknown outcome.
type Blocked struct {
	Previous Intent
}

func (b Blocked) Error() string {
	if b.Previous.State == Dispatched {
		return fmt.Sprintf("blocked: intent %s is still in progress; wait for its result before retrying", b.Previous.ID)
	}
	return fmt.Sprintf("blocked pending reconciliation: intent %s from attempt %s made this same call and its outcome is unknown; a person must resolve it with /effects %s happened|new", b.Previous.ID, b.Previous.AttemptID, b.Previous.ID)
}

type Service struct {
	l      *ledger.Ledger
	now    func() time.Time
	mu     sync.Mutex
	active map[string]bool
}

func New(l *ledger.Ledger) *Service {
	return &Service{l: l, now: time.Now, active: map[string]bool{}}
}

// Fingerprint identifies a call by what it does, not when.
func Fingerprint(tool string, args []byte) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), args...))
	return hex.EncodeToString(sum[:12])
}

// Claim opens an intent unless that task's same operation is still in flight
// or has an unknown outcome, including retries in the same attempt.
func (s *Service) Claim(ctx context.Context, taskID, attemptID, tool string, args []byte) (Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp := Fingerprint(tool, args)
	if taskID != "" {
		all, err := s.ForTask(ctx, taskID)
		if err != nil {
			return Intent{}, err
		}
		for _, prev := range all {
			if (prev.RequiresReconciliation || prev.Fingerprint == fp) && (prev.State == Unknown || prev.State == Dispatched) {
				prev, err = s.recoverDispatch(ctx, prev)
				if err != nil {
					return Intent{}, err
				}
				return Intent{}, Blocked{Previous: prev}
			}
		}
	}
	id := fmt.Sprintf("int-%s-%d", fp[:8], s.now().UnixNano())
	it := Intent{ID: id, TaskID: taskID, AttemptID: attemptID, Tool: tool, Fingerprint: fp, State: Claimed, At: s.now().UTC()}
	if _, err := s.l.Begin(ctx, id, kind, string(Claimed), attemptID, it); err != nil {
		return Intent{}, err
	}
	s.active[id] = true
	return it, nil
}

// Dispatched marks the moment before the call leaves, in the journal
// first: the record must exist before the world can be changed.
func (s *Service) Dispatched(ctx context.Context, id string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if err != nil {
			delete(s.active, id)
		}
	}()
	if _, err := s.l.Journal().Started(ledger.EffectID{Operation: id, Kind: "dispatch", InstanceKey: "1"}, "", nil); err != nil {
		return err
	}
	return s.move(ctx, id, Claimed, Dispatched, nil, "")
}

// Confirmed records the receipt.
func (s *Service) Confirmed(ctx context.Context, id string, receipt any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer delete(s.active, id)
	raw, _ := json.Marshal(receipt)
	if _, err := s.l.Journal().Confirmed(ledger.EffectID{Operation: id, Kind: "dispatch", InstanceKey: "1"}, receipt); err != nil {
		return err
	}
	return s.move(ctx, id, Dispatched, Succeeded, raw, "")
}

// Failed records a call the platform refused: it did not happen.
func (s *Service) Failed(ctx context.Context, id string, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer delete(s.active, id)
	it, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	return s.move(ctx, id, it.State, Failed, nil, cause.Error())
}

// Lost records a call whose answer never came: it may have happened.
func (s *Service) Lost(ctx context.Context, id string, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer delete(s.active, id)
	return s.move(ctx, id, Dispatched, Unknown, nil, cause.Error())
}

// Resolve is a person's word on an outcome-unknown intent: "happened"
// keeps it as done, "new" says it never did and the call may be made
// again.
func (s *Service) Resolve(ctx context.Context, id, verdict, by string) (Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	to := State("")
	switch verdict {
	case "happened":
		to = Succeeded
	case "new":
		to = Failed
	default:
		return Intent{}, fmt.Errorf("verdict %q is not happened or new", verdict)
	}
	if s.active[id] {
		return Intent{}, fmt.Errorf("intent %s is still in progress", id)
	}
	it, err := s.Get(ctx, id)
	if err != nil {
		return Intent{}, err
	}
	it, err = s.recoverDispatch(ctx, it)
	if err != nil {
		return Intent{}, err
	}
	if it.State != Unknown {
		return Intent{}, ErrNotUnknown
	}
	if err := s.move(ctx, id, Unknown, to, nil, "resolved as "+verdict+" by "+by); err != nil {
		return Intent{}, err
	}
	return s.Get(ctx, id)
}

func (s *Service) move(ctx context.Context, id string, from, to State, receipt json.RawMessage, note string) error {
	_, err := s.l.Transition(ctx, id, string(from), string(to), "intent", nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		var it Intent
		if err := json.Unmarshal(op.Data, &it); err != nil {
			return err
		}
		it.State = to
		if receipt != nil {
			it.Receipt = receipt
		}
		if note != "" {
			if to == Failed || to == Unknown {
				it.Error = note
			} else {
				it.Resolution = note
			}
		}
		return tx.SetData(op, it)
	})
	return err
}

func (s *Service) Get(ctx context.Context, id string) (Intent, error) {
	op, ok, err := s.l.Operation(ctx, id)
	if err != nil {
		return Intent{}, err
	}
	if !ok {
		return Intent{}, fmt.Errorf("intent %s not found", id)
	}
	return decode(op)
}

// ForTask lists a task's intents, oldest first.
func (s *Service) ForTask(ctx context.Context, taskID string) ([]Intent, error) {
	return s.list(ctx, func(it Intent) bool { return it.TaskID == taskID })
}

// Unresolved includes abandoned dispatches, recovered to outcome-unknown so
// they can be resolved. A live provider request never becomes recoverable.
func (s *Service) Unresolved(ctx context.Context) ([]Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates, err := s.list(ctx, func(it Intent) bool { return it.State == Unknown || it.State == Dispatched })
	if err != nil {
		return nil, err
	}
	var out []Intent
	for _, it := range candidates {
		it, err = s.recoverDispatch(ctx, it)
		if err != nil {
			return nil, err
		}
		if it.State == Unknown {
			out = append(out, it)
		}
	}
	return out, nil
}

// PendingResolution observes outcomes that need reconciliation without
// performing recovery writes. A read model may call this safely; Resolve
// performs the guarded recovery transition when the operator acts.
func (s *Service) PendingResolution(ctx context.Context) ([]Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(ctx, func(it Intent) bool {
		return it.State == Unknown || (it.State == Dispatched && !s.active[it.ID])
	})
}

// recoverDispatch runs under mu. The active set belongs to this process;
// after restart, or after a failed terminal write, a dispatched operation
// has no live caller that can establish its result.
func (s *Service) recoverDispatch(ctx context.Context, it Intent) (Intent, error) {
	if it.State != Dispatched || s.active[it.ID] {
		return it, nil
	}
	note := "operation ended before its final result was recorded"
	if err := s.move(ctx, it.ID, Dispatched, Unknown, nil, note); err != nil {
		return Intent{}, err
	}
	it.State, it.Error = Unknown, note
	return it, nil
}

func (s *Service) list(ctx context.Context, keep func(Intent) bool) ([]Intent, error) {
	ops, err := s.l.Operations(ctx, kind, "")
	if err != nil {
		return nil, err
	}
	var out []Intent
	for _, op := range ops {
		it, err := decode(op)
		if err != nil {
			return nil, err
		}
		if keep(it) {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func decode(op ledger.Operation) (Intent, error) {
	var it Intent
	if err := json.Unmarshal(op.Data, &it); err != nil {
		return Intent{}, err
	}
	it.State = State(op.State)
	return it, nil
}

var ErrNotUnknown = errors.New("intent: not outcome-unknown")

// ForAgents adapts the service to what the agent tool server needs: the
// attempt is found from the task, since an agent only knows its task.
type ForAgents struct {
	S        *Service
	Attempts AttemptSource
}

// AttemptSource finds the live attempt of a task.
type AttemptSource interface {
	LiveAttemptOf(ctx context.Context, taskID string) (string, bool)
}

func (f ForAgents) Claim(ctx context.Context, taskID, tool string, args []byte) (string, error) {
	attemptID := ""
	if f.Attempts != nil {
		if id, ok := f.Attempts.LiveAttemptOf(ctx, taskID); ok {
			attemptID = id
		}
	}
	it, err := f.S.Claim(ctx, taskID, attemptID, tool, args)
	if err != nil {
		return "", err
	}
	return it.ID, nil
}

func (f ForAgents) Dispatched(ctx context.Context, id string) error { return f.S.Dispatched(ctx, id) }
func (f ForAgents) Confirmed(ctx context.Context, id string, receipt any) error {
	return f.S.Confirmed(ctx, id, receipt)
}
func (f ForAgents) Failed(ctx context.Context, id string, cause error) error {
	return f.S.Failed(ctx, id, cause)
}
func (f ForAgents) Lost(ctx context.Context, id string, cause error) error {
	return f.S.Lost(ctx, id, cause)
}
