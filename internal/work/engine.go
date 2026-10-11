package work

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/eventlog"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/interaction"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turnapi"
	"github.com/gopact-ai/steve/internal/view"
	"sync"
	"time"
)

const InputKind = "work-input"
const InputRunning = "running"
const InputDone = "done"
const InputUnknown = "unknown"

// InputRecord is the business execution receipt, independent of presentation
// queues and native outbound commands. Source identities remain unchanged.
type InputRecord struct {
	ID               string           `json:"id"`
	Address          channel.Address  `json:"address"`
	Actor            identity.Account `json:"actor"`
	Principal        string           `json:"principal"`
	AccountRevision  uint64           `json:"account_revision"`
	Digest           string           `json:"digest"`
	State            string           `json:"state"`
	Runtime          string           `json:"runtime"`
	Task             string           `json:"task,omitempty"`
	Attempt          string           `json:"attempt,omitempty"`
	CompletedAttempt string           `json:"completed_attempt,omitempty"`
	Result           *turnapi.Result  `json:"result,omitempty"`
	Error            string           `json:"error,omitempty"`
	UserError        bool             `json:"user_error,omitempty"`
	Cancelled        bool             `json:"cancelled,omitempty"`
	At               time.Time        `json:"at"`
	Causation        string           `json:"causation,omitempty"`
}

type Engine struct {
	Book         *ledger.Ledger
	Runtime      string
	Interactions *interaction.Service
}
type Execute func(context.Context, turnapi.Request) (turnapi.Result, error)

func inputIdentity(req turnapi.Request) (string, string, error) {
	raw, err := json.Marshal(struct {
		Source        turnapi.Source
		Actor         turnapi.Actor
		Reply         turnapi.ReplyContext
		Admission     turnapi.Admission
		Input, Locale string
		Images        []harness.Media
	}{req.Source, req.Actor, req.Reply, req.Admission, req.Input, req.Locale, req.Images})
	return durable.Key(req.Source.Channel, req.Source.ConversationID, req.Source.MessageID), durable.Key(string(raw)), err
}

// Run is shared by Console, native IM, schedules and provider submissions.
// Replays observe a committed result. A running/unknown receipt never creates
// another attempt: original-execution recovery owns that input thereafter.
func (s *Engine) Run(ctx context.Context, req turnapi.Request, execute Execute) (result turnapi.Result, err error) {
	if s == nil || s.Book == nil || req.Source.MessageID == "" {
		return execute(ctx, req)
	}
	if req.Source.Channel == "" {
		req.Source.Channel = "console"
	}
	record, replay, err := s.admitInput(ctx, req)
	if err != nil {
		return result, err
	}
	id := record.ID
	if replay {
		if record.State != InputDone || record.Result == nil {
			return result, channel.ErrOutcomeUnknown
		}
		return *record.Result, replayError(record)
	}
	var scopeMu sync.Mutex
	executeCtx, cancelExecute := context.WithCancel(ctx)
	defer cancelExecute()
	var admissionErr error
	originalReady := req.OnTurnReady
	req.OnTurnReady = func(taskID, attemptID string) {
		scopeMu.Lock()
		record.Task, record.Attempt = taskID, attemptID
		scopeMu.Unlock()
		commitErr := s.bindAttempt(ctx, req, id, taskID, attemptID)
		// The original observer remains responsible for its attempt receipt.
		// Failure to record this additional fact cannot invent another attempt.
		if commitErr != nil {
			scopeMu.Lock()
			record.Error = commitErr.Error()
			admissionErr = commitErr
			scopeMu.Unlock()
			cancelExecute()
		}
		if originalReady != nil {
			originalReady(taskID, attemptID)
		}
	}
	origin := func() interaction.Origin {
		scopeMu.Lock()
		defer scopeMu.Unlock()
		return interaction.Origin{Work: id, Address: req.Address(), Actor: record.Actor, Task: record.Task, Attempt: record.Attempt}
	}
	if downstream := req.OnAsk; downstream != nil && s.Interactions != nil {
		req.OnAsk = func(ctx context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			out, err := s.Interactions.Request(ctx, origin(), "permission", ask, func() (any, error) { return downstream(ctx, ask) })
			if err != nil {
				return permission.Choose(false, ask.Options), err
			}
			return out.(acp.RequestPermissionOutcome), nil
		}
	}
	if downstream := req.OnAskUser; downstream != nil && s.Interactions != nil {
		req.OnAskUser = func(ctx context.Context, q view.Question) (view.Answer, error) {
			out, err := s.Interactions.Request(ctx, origin(), "question", q, func() (any, error) { return downstream(ctx, q) })
			if err != nil {
				return view.Answer{}, err
			}
			return out.(view.Answer), nil
		}
	}
	result, err = execute(executeCtx, req)
	scopeMu.Lock()
	err = errors.Join(err, admissionErr)
	blocked := admissionErr != nil
	scopeMu.Unlock()
	if blocked {
		return result, errors.Join(channel.ErrOutcomeUnknown, err)
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	commitErr := s.Book.Update(finish, func(tx *ledger.Tx) error { return CompleteInputTx(tx, req.Address(), result, err) })
	if commitErr != nil {
		return turnapi.Result{}, errors.Join(channel.ErrOutcomeUnknown, err, commitErr)
	}
	return result, err
}

func replayError(record InputRecord) error {
	if record.Cancelled {
		return context.Canceled
	}
	if record.Error == "" {
		return nil
	}
	if record.UserError {
		return turnapi.UserError{Text: record.Error}
	}
	return errors.New(record.Error)
}

// CompleteInputTx joins retained recovery with the same input receipt. It is
// called inside an adapter's original completion transaction when available.
func CompleteInputTx(tx *ledger.Tx, address channel.Address, result turnapi.Result, cause error) error {
	id := durable.Key(address.Channel, address.Conversation, address.Message)
	record, found, err := durable.Get[InputRecord](tx, InputKind, id)
	if err != nil || !found {
		return err
	}
	if record.State == InputDone {
		return nil
	}
	if record.Attempt == "" && result.Attempt != "" {
		a, err := attempt.GetTx(tx, result.Attempt)
		if err != nil {
			return err
		}
		tracked, found, err := task.GetTx(tx, a.TaskID)
		if err != nil {
			return err
		}
		if !found || a.By != record.Actor.ID || tracked.Transport != record.Actor.Channel || tracked.Channel != record.Address.Conversation || a.TurnID != record.Address.Message {
			return identity.ErrDenied
		}
		record.Task, record.Attempt = a.TaskID, a.ID
	}
	if record.Attempt != "" {
		a, err := completionAttempt(tx, record, result.Attempt)
		if err != nil {
			return err
		}
		if a.TaskID != record.Task {
			return identity.ErrDenied
		}
		if !a.State.Terminal() || a.Unsettled || errors.Is(cause, harness.ErrStopUnconfirmed) {
			record.State = InputUnknown
			return tx.PutBinding(InputKind, id, record)
		}
		record.CompletedAttempt = a.ID
	}
	record.State, record.Result = InputDone, &result
	if cause != nil {
		record.Error = cause.Error()
		var user turnapi.UserError
		record.UserError = errors.As(cause, &user)
		record.Cancelled = errors.Is(cause, context.Canceled)
	} else {
		record.Error = ""
	}
	if err := tx.PutBinding(InputKind, id, record); err != nil {
		return err
	}
	raw, _ := json.Marshal(struct{ Work, Task, Attempt, CompletedAttempt string }{record.ID, record.Task, record.Attempt, record.CompletedAttempt})
	depth, err := eventlog.DepthAfter(tx, record.Causation)
	if err != nil {
		return err
	}
	_, err = eventlog.AppendTx(tx, eventlog.Event{Source: "server:work", Key: record.ID, Type: "work.completed", Subject: record.Address.Conversation, Payload: raw, Causation: record.Causation, Depth: depth, At: time.Now().UTC()})
	return err
}

func (s *Engine) admitInput(ctx context.Context, req turnapi.Request) (InputRecord, bool, error) {
	id, digest, err := inputIdentity(req)
	if err != nil {
		return InputRecord{}, false, err
	}
	record := InputRecord{ID: id, Address: req.Address(), Actor: identity.Account{Channel: req.Source.Channel, ID: req.Actor.ID}, Digest: digest, State: InputRunning, Runtime: s.Runtime, At: time.Now().UTC()}
	replay := false
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		active, found, err := durable.Get[string](tx, interaction.RuntimeKind, "active")
		if err != nil {
			return err
		}
		if found && active != s.Runtime {
			return ledger.ErrStale
		}
		account, err := identity.ResolveAccount(tx, record.Actor)
		if err != nil {
			return err
		}
		record.Principal, record.AccountRevision = account.Principal, account.Revision
		old, found, err := durable.Get[InputRecord](tx, InputKind, id)
		if err != nil {
			return err
		}
		if found {
			if old.Digest != digest || old.Actor != record.Actor {
				return ledger.ErrConflict
			}
			if old.Principal != account.Principal || old.AccountRevision != account.Revision {
				return identity.ErrDenied
			}
			if old.State == InputDone && old.Attempt != "" {
				if _, configured, err := durable.Get[string](tx, identity.AuthorityKind, "owner"); err != nil {
					return err
				} else if configured {
					a, err := attempt.GetTx(tx, old.Attempt)
					if err != nil {
						return err
					}
					owner, _, err := durable.Get[string](tx, identity.AuthorityKind, "owner")
					if err != nil {
						return err
					}
					role, err := project.AccessPolicyTx(tx, a.Project, old.Principal, owner)
					if err != nil {
						return err
					}
					if !role.AtLeast(project.RoleRead) {
						return identity.ErrDenied
					}
				}
			}
			record = old
			replay = true
			return nil
		}
		if origin := req.Source.Origin; len(origin) > len("automation:") && origin[:len("automation:")] == "automation:" {
			w, found, err := durable.Get[Record](tx, RecordKind, origin[len("automation:"):])
			if err != nil {
				return err
			}
			if found {
				record.Causation = w.Request.Causation
			}
		}
		return tx.PutBinding(InputKind, id, record)
	})
	return record, replay, err
}

func (s *Engine) bindAttempt(ctx context.Context, req turnapi.Request, id, taskID, attemptID string) error {
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return s.Book.Update(commit, func(tx *ledger.Tx) error {
		current, found, err := durable.Get[InputRecord](tx, InputKind, id)
		if err != nil {
			return err
		}
		if !found || current.Runtime != s.Runtime || current.State != InputRunning {
			return ledger.ErrConflict
		}
		a, err := attempt.GetTx(tx, attemptID)
		if err != nil {
			return err
		}
		if a.TaskID != taskID || a.TurnID != req.Source.MessageID {
			return identity.ErrDenied
		}
		if a.Execution == nil || a.State.Terminal() {
			return identity.ErrDenied
		}
		if err := task.CheckExecutionTx(tx, a.Execution); err != nil {
			return err
		}
		if err := BindExecutionAccountTx(tx, attemptID, req.Address(), current.Actor); err != nil {
			return err
		}
		tracked, found, err := task.GetTx(tx, taskID)
		if err != nil {
			return err
		}
		if !found {
			return identity.ErrDenied
		}
		if a.By != current.Actor.ID || tracked.Transport != current.Actor.Channel || tracked.Channel != req.Source.ConversationID || tracked.Member != a.Agent {
			return identity.ErrDenied
		}
		if tracked.Requester == current.Actor.ID && tracked.Transport == current.Actor.Channel {
			if err := identity.PinTaskTx(tx, taskID, attemptID, a.Execution.Epoch); err != nil {
				return err
			}
		}
		if current.Attempt != "" {
			if _, err := completionAttempt(tx, current, attemptID); err != nil {
				return err
			}
		} else {
			current.Task, current.Attempt = taskID, attemptID
		}
		return tx.PutBinding(InputKind, id, current)
	})
}
