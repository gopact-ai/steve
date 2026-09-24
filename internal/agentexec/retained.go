package agentexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

// RecoveryBlocked leaves the original attempt and input in place. Its question
// is a platform diagnostic, never an answer to a native permission request.
// The execution layer names the attempt and task it could not settle; a
// question raised above it, for a whole exchange, names neither.
//
// Transports (gateway, console) match any *RecoveryBlocked. An execution
// layer block reaches them only through turn's planRecoveryError, which
// drops its AttemptID and TaskID and joins harness.ErrStopUnconfirmed to
// its cause; a block that still names an attempt must not leave turn.
type RecoveryBlocked struct {
	AttemptID, TaskID string
	Question          view.Question
	Cause             error

	// diagnosis keeps what the execution layer found, so that the question
	// can be asked again in the language of the exchange it reaches.
	diagnosis *diagnosed
}

type diagnosed struct {
	record attempt.Record
	code   string
	Diagnosis
}

func (e *RecoveryBlocked) Error() string            { return e.Question.Message }
func (e *RecoveryBlocked) Unwrap() error            { return e.Cause }
func (e *RecoveryBlocked) UnsettledAttempt() string { return e.AttemptID }

// In is the block asked in text's language. A block the execution layer
// raised is asked again from its diagnosis; any other is returned as is.
func (e *RecoveryBlocked) In(text i18n.Catalog) *RecoveryBlocked {
	if e == nil || e.diagnosis == nil {
		return e
	}
	return BlockedIn(text, e.diagnosis.record, e.diagnosis.code, e.diagnosis.Diagnosis, e.Cause)
}

// RecoveryQuestion is the question every recovery block asks: check again,
// as retry describes, or wait with the task and its progress kept.
func RecoveryQuestion(requestID, title, message string, retry, wait view.Choice) view.Question {
	retry.Value, wait.Value = "retry", "wait"
	return view.Question{RequestID: requestID, Kind: "recovery", Title: title, Message: message, Required: true, AllowFreeText: true, Choices: []view.Choice{retry, wait}}
}

// Diagnosis is what the execution layer tried for an attempt, what stops
// it, and what the owner may do about it.
type Diagnosis struct {
	Attempted, Problem, Recommendation i18n.Key
}

// Blocked is BlockedIn English: the execution layer has no request of its
// own to take a language from. The exchange asks it in its own through In.
func Blocked(record attempt.Record, code string, d Diagnosis, cause error) *RecoveryBlocked {
	return BlockedIn(i18n.New(i18n.LocaleEN), record, code, d, cause)
}

// BlockedIn is the block of an attempt the execution layer could not
// settle, asked in text's language.
func BlockedIn(text i18n.Catalog, record attempt.Record, code string, d Diagnosis, cause error) *RecoveryBlocked {
	problem := text.T(d.Problem)
	if cause != nil {
		problem += "\n" + text.T(i18n.RecoveryDiagnostics, cause.Error())
	}
	message := text.T(i18n.RecoveryAttempted, text.T(d.Attempted)) + "\n\n" + problem + "\n\n" + text.T(i18n.RecoveryStopUnconfirmed) + "\n\n" + text.T(d.Recommendation)
	question := RecoveryQuestion("execution-recovery/"+record.ID+"/"+code, text.T(i18n.RecoveryTitleExecution), message,
		view.Choice{Label: text.T(i18n.RecoveryRetry), Detail: text.T(i18n.RecoveryRetryExecution)},
		view.Choice{Label: text.T(i18n.RecoveryWait), Detail: text.T(i18n.RecoveryWaitConditions)})
	return &RecoveryBlocked{AttemptID: record.ID, TaskID: record.TaskID, Cause: cause, Question: question,
		diagnosis: &diagnosed{record: record, code: code, Diagnosis: d}}
}

type auxiliaryInput struct {
	Spec   Spec   `json:"spec"`
	Prompt string `json:"prompt"`
}
type auxiliaryOutput struct {
	Input      *auxiliaryInput `json:"input,omitempty"`
	WorkID     string          `json:"work_id"`
	Answer     string          `json:"answer"`
	Error      string          `json:"error,omitempty"`
	Validation bool            `json:"validation,omitempty"`
}
type retainedSessions interface {
	AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error)
}

func workID(spec Spec, prompt string) (string, error) {
	raw, err := json.Marshal(struct {
		Task, Turn, Agent, Project, Base, Kind, Prompt string
		Source                                         json.RawMessage
	}{spec.TaskID, spec.TurnID, spec.Agent, spec.Project, spec.Base, string(spec.Kind), prompt, spec.Source})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func auxiliaryKey(spec Spec) string {
	return spec.TaskID + "\x00" + string(spec.Kind) + "\x00" + spec.TurnID
}
func (r *Runner) claim(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = map[string]bool{}
	}
	if r.active[key] {
		return false
	}
	r.active[key] = true
	return true
}
func (r *Runner) release(key string) { r.mu.Lock(); delete(r.active, key); r.mu.Unlock() }
func (r *Runner) SetQuestionHandlers(ask permission.AskFunc, askUser acphost.AskUserFunc) {
	r.mu.Lock()
	r.ask, r.askUser = ask, askUser
	r.mu.Unlock()
}
func (r *Runner) SetObserver(observe func(attempt.Record, view.Progress)) {
	r.mu.Lock()
	r.observe = observe
	r.mu.Unlock()
}
func (r *Runner) callbacks() (permission.AskFunc, acphost.AskUserFunc, func(attempt.Record, view.Progress)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ask, r.askUser, r.observe
}

// ResumeAttempt consumes only the native command already linked to this
// attempt. Its caller validates the original task/exchange identity first.
func (r *Runner) ResumeAttempt(ctx context.Context, id string, validate func(string) error) (Result, error) {
	record, err := r.attempts.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	spec := Spec{TaskID: record.TaskID, TurnID: record.TurnID, Kind: record.Kind}
	key := auxiliaryKey(spec)
	if !r.claim(key) {
		return Result{}, Blocked(record, "busy", Diagnosis{Attempted: i18n.ExecTriedObserver, Problem: i18n.ExecProblemObserved, Recommendation: i18n.ExecAdviceAwaitObserver}, nil)
	}
	defer r.release(key)
	return r.resumeAttempt(ctx, record, validate)
}
func (r *Runner) resumeAttempt(parent context.Context, record attempt.Record, validate func(string) error) (out Result, runErr error) {
	out.Attempt = record
	if _, err := originalInput(record); err != nil {
		return out, Blocked(record, "input", Diagnosis{Attempted: i18n.ExecTriedReadRequest, Problem: i18n.ExecProblemRequestUnreadable, Recommendation: i18n.ExecAdviceCheckRecords}, err)
	}
	if (record.Kind != attempt.KindPlan && record.Kind != attempt.KindVerify) || !nodewire.IsManagedSession(record.Session) || record.Execution == nil || record.WorkID == "" {
		return out, Blocked(record, "identity", Diagnosis{Attempted: i18n.ExecTriedAuxIdentity, Problem: i18n.ExecProblemNoIdentity, Recommendation: i18n.ExecAdviceCheckTask}, nil)
	}
	scope, err := r.executions.BeginAccepted(parent, execution.Key{TaskID: record.TaskID, InstanceID: record.TurnID, AttemptID: record.ID}, record.Execution)
	if err != nil {
		return out, err
	}
	var unresolved error
	defer func() { scope.Finish(unresolved) }()
	ctx := scope.Context()
	if record.State.Terminal() {
		if record.Unsettled {
			return out, Blocked(record, "unsettled", Diagnosis{Attempted: i18n.ExecTriedStopRecord, Problem: i18n.ExecProblemStopUnconfirmed, Recommendation: i18n.ExecAdviceCheckNodeEffects}, harness.ErrStopUnconfirmed)
		}
		if err := SettleBudget(r.budget, record, nil); err != nil {
			return out, Blocked(record, "accounting", Diagnosis{Attempted: i18n.ExecTriedBudgetRecord, Problem: i18n.ExecProblemEndedUnsettled, Recommendation: i18n.ExecAdviceRestoreStorageCheck}, err)
		}
		if err := r.cleanupAuxiliary(ctx, record); err != nil {
			return out, Blocked(record, "cleanup", Diagnosis{Attempted: i18n.ExecTriedReleaseWorkspace, Problem: i18n.ExecProblemWorkspaceHeld, Recommendation: i18n.ExecAdviceReconnectNode}, err)
		}
		return decodeAuxiliary(record, validate)
	}
	retain := func(code string, cause error) (Result, error) {
		unresolved = &execution.RetainedObserverDetached{AttemptID: record.ID, NodeID: record.Node, SessionID: record.Session, Cause: errors.Join(harness.ErrStopUnconfirmed, cause)}
		return out, Blocked(record, code, Diagnosis{Attempted: i18n.ExecTriedMatchIdentity, Problem: i18n.ExecProblemAuxUnsafe, Recommendation: i18n.ExecAdviceRestoreEither}, unresolved)
	}
	manager, ok := r.sessions.(retainedSessions)
	if !ok {
		return retain("runtime", errors.New("retained session interface unavailable"))
	}
	session, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session, record.Workspace.Path)
	if err != nil {
		return retain("attach", err)
	}
	inspector, ok := session.(harness.RetainedSessionInspector)
	if !ok {
		return retain("inspect", errors.New("retained state inspection unavailable"))
	}
	observed, err := inspector.InspectRetained(ctx)
	if err != nil {
		return retain("inspect", err)
	}
	refreshed, err := r.attempts.RecoverRetained(ctx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: observed})
	if err != nil {
		return retain("authority", err)
	}
	record = refreshed
	out.Attempt = record
	scope.AdoptRetained()
	work := &auxiliary{runner: r, spec: Spec{TaskID: record.TaskID, TurnID: record.TurnID, Kind: record.Kind, Agent: record.Agent, Project: record.Project}, identity: record.WorkID, validate: validate, running: record, reserved: true}
	ask, askUser, observe := r.callbacks()
	options := lifecycle.Options{
		Attempts: r.attempts, Roster: r.roster, Sessions: r.sessions, Workspaces: r.workspaces, Actor: "agentexec",
		Spec: record.Spec, At: harness.Placement{Node: record.Node, Harness: record.Harness},
		Resume: true, Ask: ask, AskUser: askUser,
		Observe: func(p view.Progress) {
			EmitProgress(ctx, p)
			if observe != nil {
				observe(record, p)
			}
		},
		Validate: work.check, Finish: work.finish, Failed: work.failed,
		Settlement: auxiliarySettlement,
	}
	if record.State != attempt.Running && record.Result != nil && len(record.Result.Output) > 0 {
		// The command already ended and its output is on the record: the
		// completion is rebuilt from it, never from a second prompt.
		var saved auxiliaryOutput
		if json.Unmarshal(record.Result.Output, &saved) != nil || saved.WorkID != record.WorkID {
			return retain("output", errors.New("candidate output identity differs"))
		}
		var nativeErr error
		if saved.Error != "" {
			nativeErr = errors.New(saved.Error)
			if saved.Validation {
				work.invalid = nativeErr
			}
		}
		if nativeErr == nil && validate != nil {
			if err := validate(saved.Answer); err != nil {
				return retain("validation", err)
			}
		}
		options.Replay, options.Validate = &lifecycle.Outcome{Answer: saved.Answer, PromptSettled: true, Err: nativeErr}, nil
	}
	run, err := lifecycle.Reattach(ctx, options, record, session)
	out.Attempt, out.Usage, out.Answer = run.Record, run.Usage, run.Answer
	runErr, unresolved = work.settle(run, err)
	return out, runErr
}

func decodeAuxiliary(record attempt.Record, validate func(string) error) (out Result, err error) {
	out.Attempt, out.Usage = record, record.Usage
	var saved auxiliaryOutput
	if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &saved) != nil || saved.WorkID != record.WorkID {
		return out, Blocked(record, "output", Diagnosis{Attempted: i18n.ExecTriedReadOutput, Problem: i18n.ExecProblemOutputMissing, Recommendation: i18n.ExecAdviceCheckOutput}, nil)
	}
	out.Answer = saved.Answer
	if saved.Validation {
		return out, &ValidationError{Cause: errors.New(saved.Error)}
	}
	if saved.Error != "" {
		return out, errors.New(saved.Error)
	}
	if record.State != attempt.Bound {
		return out, Blocked(record, "state", Diagnosis{Attempted: i18n.ExecTriedCommitState, Problem: i18n.ExecProblemNotCommitted, Recommendation: i18n.ExecAdviceCheckExecution}, nil)
	}
	if validate != nil {
		if err := validate(out.Answer); err != nil {
			return out, Blocked(record, "validation", Diagnosis{Attempted: i18n.ExecTriedRevalidate, Problem: i18n.ExecProblemValidationChanged, Recommendation: i18n.ExecAdviceConfirmConditions}, err)
		}
	}
	return out, nil
}

func (r *Runner) cleanupAuxiliary(ctx context.Context, record attempt.Record) error {
	cleanup, stop := lifecycle.Cleanup(ctx)
	defer stop()
	if err := r.sessions.CloseSession(cleanup, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session); err != nil {
		return fmt.Errorf("close settled auxiliary session: %w", err)
	}
	r.roster.Release(cleanup, record.Node, record.ID)
	if err := r.workspaces.Discard(cleanup, record.Workspace); err != nil {
		return fmt.Errorf("discard auxiliary workspace: %w", err)
	}
	return nil
}
func clipAnswer(answer string) string {
	r := []rune(answer)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}

func originalInput(record attempt.Record) (auxiliaryInput, error) {
	var output auxiliaryOutput
	if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &output) != nil || output.Input == nil {
		return auxiliaryInput{}, errors.New("original auxiliary request unavailable")
	}
	input := *output.Input
	id, err := workID(input.Spec, input.Prompt)
	if err != nil || id != record.WorkID || output.WorkID != record.WorkID || input.Spec.TaskID != record.TaskID || input.Spec.TurnID != record.TurnID || input.Spec.Project != record.Project || input.Spec.Agent != record.Agent || input.Spec.Kind != record.Kind {
		return auxiliaryInput{}, errors.New("original auxiliary request identity differs")
	}
	return input, nil
}
func (r *Runner) OriginalSpec(ctx context.Context, id string) (Spec, error) {
	record, err := r.attempts.Get(ctx, id)
	if err != nil {
		return Spec{}, err
	}
	input, err := originalInput(record)
	return input.Spec, err
}
