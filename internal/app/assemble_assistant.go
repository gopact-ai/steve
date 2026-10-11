package app

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/clientcap"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/events"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/serviceapi"
	"github.com/gopact-ai/steve/internal/servicehub"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/work"
)

func assembleAssistant(boot runtimeAssembly, page consoleAssembly, delegates delegationAssembly) {
	services := servicehub.New(boot.Book(), boot.Config().EffectiveOwnerID(), page.Console())
	services.Clients.CheckExecution = func(tx *ledger.Tx, call clientcap.Invocation) error {
		return checkClientExecution(tx, services.Identity.OwnerID, call)
	}
	services.Clients.ExecutionDepth = func(r ledger.Reader, execution *clientcap.Execution) (int, error) {
		t, found, err := task.GetTx(r, execution.Task)
		if err != nil {
			return 0, err
		}
		if !found || !strings.HasPrefix(t.Origin, "automation:") {
			return 1, nil
		}
		w, found, err := durable.Get[work.Record](r, work.RecordKind, strings.TrimPrefix(t.Origin, "automation:"))
		if err != nil {
			return 0, err
		}
		if !found {
			return 8, nil
		}
		return events.DepthAfter(r, w.Request.Causation)
	}
	page.Dashboard().SetAssistant(&serviceapi.Services{Identity: services.Identity, Events: services.Events, Work: services.Work, Clients: services.Clients})
	if gate := delegates.Gate(); gate != nil {
		gate.SetClientInvoker(&clientCapabilityTools{services: services})
	}
	boot.Background().Go(services.Run)
}

// checkClientExecution uses the ORIGINAL attempt, epoch and process generation.
// A later running attempt cannot inherit pending calls from a stopped execution.
func checkClientExecution(tx *ledger.Tx, owner string, call clientcap.Invocation) error {
	e := call.Execution
	if e == nil {
		return identity.ErrDenied
	}
	r, err := attempt.GetTx(tx, e.Attempt)
	if err != nil {
		return err
	}
	if r.State != attempt.Running || r.Unsettled || r.SessionSettled == nil || *r.SessionSettled || r.TaskID != e.Task || r.Execution == nil || r.Execution.Epoch != e.Epoch || attempt.SessionExecutionEpoch(r) != e.Generation {
		return identity.ErrDenied
	}
	if r.Node == "" {
		if err := checkLocalMCPRuntime(tx, e.LocalRuntime); err != nil {
			return identity.ErrDenied
		}
	} else if e.LocalRuntime != "" {
		return identity.ErrDenied
	}
	t, found, err := task.GetTx(tx, e.Task)
	if err != nil {
		return err
	}
	if !found || !t.State.Holds() || t.Transport != "console" || t.Channel != call.Request.Conversation || t.Requester != owner {
		return identity.ErrDenied
	}
	if err := task.CheckExecutionTx(tx, r.Execution); err != nil {
		return err
	}
	for _, lease := range r.Leases {
		if lease.Key == "attempt:"+r.ID && lease.Holder == r.ID {
			return tx.CheckLocalLease(lease)
		}
	}
	return identity.ErrDenied
}

type clientCapabilityTools struct{ services *servicehub.Services }

func (c *clientCapabilityTools) authorize(ctx context.Context, tx *ledger.Tx, bind agentmcp.Binding) error {
	scope, ok := agentmcp.ScopeFromContext(ctx)
	if !ok || bind.TaskID != "" || bind.DelegatedBy != "" {
		return identity.ErrDenied
	}
	if err := agentmcp.AuthorizeContext(ctx, applicationMCPTx{tx}); err != nil {
		return err
	}
	call := clientcap.Invocation{Request: clientcap.Request{Conversation: bind.ConversationID}, Execution: &clientcap.Execution{Task: scope.TaskID, Attempt: scope.AttemptID, Epoch: scope.TaskEpoch, Generation: scope.ExecutionGeneration, LocalRuntime: scope.LocalRuntime}}
	return checkClientExecution(tx, c.services.Identity.OwnerID, call)
}
func (c *clientCapabilityTools) Available(ctx context.Context, bind agentmcp.Binding) ([]clientcap.Registration, error) {
	if err := c.services.Book.Update(ctx, func(tx *ledger.Tx) error { return c.authorize(ctx, tx, bind) }); err != nil {
		return nil, err
	}
	return c.services.Clients.Available(ctx, identity.Principal{Owner: true}, bind.ConversationID)
}
func (c *clientCapabilityTools) Invoke(ctx context.Context, bind agentmcp.Binding, input agentmcp.ClientInvoke) (clientcap.Invocation, error) {
	scope, ok := agentmcp.ScopeFromContext(ctx)
	if !ok {
		return clientcap.Invocation{}, identity.ErrDenied
	}
	call, err := c.services.Clients.Submit(ctx, identity.Principal{Owner: true}, clientcap.Request{Key: input.Key, Client: input.Client, Capability: input.Capability, Conversation: bind.ConversationID, Arguments: input.Arguments}, &clientcap.Execution{Task: scope.TaskID, Attempt: scope.AttemptID, Epoch: scope.TaskEpoch, Generation: scope.ExecutionGeneration, LocalRuntime: scope.LocalRuntime}, func(tx *ledger.Tx) error { return c.authorize(ctx, tx, bind) })
	call.Token = ""
	return call, err
}
func (c *clientCapabilityTools) Result(ctx context.Context, bind agentmcp.Binding, id string) (clientcap.Invocation, error) {
	scope, ok := agentmcp.ScopeFromContext(ctx)
	if !ok {
		return clientcap.Invocation{}, identity.ErrDenied
	}
	if err := c.services.Book.Update(ctx, func(tx *ledger.Tx) error { return c.authorize(ctx, tx, bind) }); err != nil {
		return clientcap.Invocation{}, err
	}
	call, err := c.services.Clients.Status(ctx, identity.Principal{Owner: true}, id)
	if err != nil {
		return call, err
	}
	if call.Execution == nil || call.Execution.Task != scope.TaskID || call.Request.Conversation != bind.ConversationID {
		return clientcap.Invocation{}, errors.New("client invocation belongs to another task or conversation")
	}
	return call, nil
}
