package delegate

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// SetSpawnGuard validates a tool's fixed grant in the same transaction as its
// child creation or result consumption. The main adapter supplies the grant store.
func (s *Service) SetSpawnGuard(guard func(context.Context, *ledger.Tx) error) {
	s.mu.Lock()
	s.spawnGuard = guard
	s.mu.Unlock()
}
func (s *Service) fixedGuard(ctx context.Context) (func(*ledger.Tx) error, error) {
	s.mu.Lock()
	guard := s.spawnGuard
	s.mu.Unlock()
	if guard == nil {
		return nil, errors.New("fixed delegation grant guard unavailable")
	}
	return func(tx *ledger.Tx) error { return guard(ctx, tx) }, nil
}
func (s *Service) parentTask(ctx context.Context, conversation, agent string) (task.Task, *task.ExecutionToken, error) {
	fixed, ok := agentmcp.ScopeFromContext(ctx)
	if !ok {
		parent, found := s.tasks.Running(conversation, agent)
		if !found {
			return task.Task{}, nil, fmt.Errorf("%s has no running task in this conversation", agent)
		}
		return parent, nil, nil
	}
	parent, err := s.parentForScope(ctx, conversation, agent, fixed)
	if err != nil {
		return task.Task{}, nil, err
	}
	token := task.ExecutionToken{TaskID: fixed.TaskID, Epoch: fixed.TaskEpoch}
	guard, err := s.fixedGuard(ctx)
	if err != nil {
		return task.Task{}, nil, err
	}
	if err := s.tasks.CheckAuthorized(ctx, token, guard); err != nil {
		return task.Task{}, nil, err
	}
	return parent, &token, nil
}
func (s *Service) parentForScope(ctx context.Context, conversation, agent string, fixed agentmcp.GrantScope) (task.Task, error) {
	parent, ok := s.tasks.Get(fixed.TaskID)
	if !ok || parent.Channel != conversation || parent.Member != agent || parent.ExecutionEpoch != fixed.TaskEpoch {
		return task.Task{}, errors.New("delegation belongs to another admitted parent execution")
	}
	current, ok := s.tasks.Running(conversation, agent)
	if !ok || current.ID != fixed.TaskID {
		return task.Task{}, errors.New("delegation cannot adopt another running parent")
	}
	token := task.ExecutionToken{TaskID: fixed.TaskID, Epoch: fixed.TaskEpoch}
	if err := s.tasks.CheckExecution(token); err != nil {
		return task.Task{}, err
	}
	if s.attempts == nil {
		return task.Task{}, errors.New("delegation attempt records unavailable")
	}
	record, err := s.attempts.Get(ctx, fixed.AttemptID)
	if err != nil {
		return task.Task{}, err
	}
	if record.TaskID != fixed.TaskID || record.Execution == nil || record.Execution.Epoch != fixed.TaskEpoch || record.Agent != agent || record.Node != fixed.NodeID || record.Session != fixed.SessionID || attempt.SessionExecutionEpoch(record) != fixed.ExecutionGeneration || record.State != attempt.Running || record.Unsettled {
		return task.Task{}, errors.New("delegation fixed native scope changed")
	}
	return parent, nil
}
func (s *Service) collectContext(ctx context.Context, id string, result agentmcp.DelegateResult) error {
	if result.State != task.StateDone && result.State != task.StateFailed {
		return nil
	}
	fixed, ok := agentmcp.ScopeFromContext(ctx)
	if !ok {
		s.collect(id, result)
		return nil
	}
	guard, err := s.fixedGuard(ctx)
	if err != nil {
		return err
	}
	return s.tasks.SetDeliveryAuthorized(ctx, task.ExecutionToken{TaskID: fixed.TaskID, Epoch: fixed.TaskEpoch}, id, task.DeliveryDelivered, guard)
}
