package turn

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/view"
)

// retainedTurn is a chat turn this process joins in flight: a node-owned
// session found again by its receipt, or a replacement opened on a
// relocation plan. It shares the chat turn's completion; the session is
// the conversation's and stays open.
type retainedTurn struct {
	c        *Coordinator
	req      Request
	record   attempt.Record
	spent    *turnSpend
	injected *Injected
	// finishing says the prompt's end is reported as a phase: a resumed
	// turn does, a relocated one did not.
	finishing bool
	finished  bool
	pending   *project.Project
	result    Result
}

// retainedSettlement: the node keeps the session and the conversation
// keeps its state. An unconfirmed stop, or a cancelled observer of a
// prompt that did not settle, detaches and quarantines the record unless
// the cancellation is why; a settled prompt is recorded on a detached
// context; what cannot be committed is rejected, as a chat turn's is.
var retainedSettlement = lifecycle.Settlement{DetachManaged: true, Detachment: lifecycle.DetachQuarantinesUnlessCancelled, RejectManaged: true, KeepSession: true, CommitTimeout: 2 * time.Minute}

func (t *retainedTurn) options(prompt string, resume bool) lifecycle.Options {
	c, req, record := t.c, t.req, t.record
	var fleet lifecycle.Roster
	if c.fleet != nil {
		fleet = c.fleet
	}
	return lifecycle.Options{
		Attempts: c.attempts, Roster: fleet, Sessions: c.runtime, Actor: "turn",
		Spec: record.Spec, At: harness.Placement{Node: record.Node, Harness: record.Harness},
		Prompt: prompt, Media: req.Images, TurnPrompt: true, Resume: resume, Ask: req.OnAsk, AskUser: req.OnAskUser, Observe: req.OnProgress,
		Ended: t.ended, Finish: t.finish,
		Settlement: retainedSettlement,
	}
}

func (t *retainedTurn) ended(e *lifecycle.Execution) {
	if t.finishing && e.Outcome.PromptSettled {
		t.req.phase(view.PhaseFinishing)
	}
}

func (t *retainedTurn) finish(ctx context.Context, e *lifecycle.Execution) (attempt.Completion, error) {
	t.finished = true
	t.result = Result{AgentID: t.record.Agent, Text: e.Outcome.Answer, Activity: e.Outcome.Activity, Attempt: t.record.ID, Injected: t.injected}
	completion, pending, err := t.c.completion(ctx, e.Record, t.result, t.spent.attemptUsage(), nil)
	t.pending = pending
	return completion, err
}

// settle reads how the reattached run ended: the result to deliver, a
// cleanup failure that leaves the execution unresolved, and the prompt's
// own error.
func (t *retainedTurn) settle(parent context.Context, run lifecycle.Result, err error) (Result, error, error) {
	c := t.c
	result := t.result
	if !t.finished {
		result = Result{AgentID: t.record.Agent, Text: run.Answer, Activity: run.Activity, Attempt: t.record.ID, Injected: t.injected}
	}
	var detached *execution.RetainedObserverDetached
	if errors.As(err, &detached) {
		var step *lifecycle.StepError
		if errors.As(err, &step) && step.Step == lifecycle.StepSettle {
			// The prompt settled on the node, but the marker saying so
			// could not be written: a cleanup that failed, which the
			// execution scope keeps so a later stop still reports it.
			return result, err, err
		}
		return result, nil, err
	}
	if c.afterTurn != nil && t.record.TaskID != "" {
		// Last of all — after the attempt is closed and the queued
		// landings are done — whoever waits for this turn's end is told.
		defer c.afterTurn(t.record.TaskID)
	}
	if !run.Durable {
		return Result{}, err, err
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Minute)
		defer cancel()
		if afterErr := c.afterCompletion(ctx, run.Record, result, t.pending, nil); afterErr != nil {
			return Result{}, afterErr, nil
		}
	}
	return result, nil, err
}
