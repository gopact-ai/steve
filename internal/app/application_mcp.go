package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

type applicationMCPStore struct{ book *ledger.Ledger }
type applicationMCPTx struct{ tx *ledger.Tx }

func newApplicationMCPStore(book *ledger.Ledger) agentmcp.Store {
	return &applicationMCPStore{book: book}
}

func (s *applicationMCPStore) Update(ctx context.Context, fn func(agentmcp.StoreTx) error) error {
	if s.book == nil {
		return errors.New("agent MCP ledger is unavailable")
	}
	return s.book.Update(ctx, func(tx *ledger.Tx) error { return fn(applicationMCPTx{tx}) })
}
func (t applicationMCPTx) Get(kind, id string, value any) (bool, error) {
	var raw string
	err := t.tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, "agent-mcp-"+kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), value)
}
func (t applicationMCPTx) Put(kind, id string, value any) error {
	return t.tx.PutBinding("agent-mcp-"+kind, id, value)
}
func (t applicationMCPTx) Delete(kind, id string) error {
	_, err := t.tx.Exec(`DELETE FROM bindings WHERE kind = ? AND id = ?`, "agent-mcp-"+kind, id)
	return err
}

func (t applicationMCPTx) record(scope agentmcp.GrantScope) (attempt.Record, error) {
	var raw, state string
	var record attempt.Record
	err := t.tx.QueryRow(`SELECT state, data FROM operations WHERE id = ? AND kind = 'attempt'`, scope.AttemptID).Scan(&state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return record, agentmcp.ErrGrantDenied
	}
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return record, err
	}
	if record.ID != scope.AttemptID || record.TaskID != scope.TaskID || record.Execution == nil || record.Execution.TaskID != scope.TaskID || record.Execution.Epoch != scope.TaskEpoch || record.Node != scope.NodeID || record.Session != scope.SessionID || attempt.SessionExecutionEpoch(record) != scope.ExecutionGeneration || string(record.State) != state {
		return record, agentmcp.ErrGrantDenied
	}
	return record, nil
}

func (t applicationMCPTx) Authorize(binding agentmcp.Binding, scope agentmcp.GrantScope) error {
	return t.check(binding, scope, false)
}

func (t applicationMCPTx) check(binding agentmcp.Binding, scope agentmcp.GrantScope, retained bool) error {
	record, err := t.record(scope)
	if err != nil {
		return err
	}
	if record.Unsettled || record.SessionSettled == nil || record.SupersededBy != "" || record.Agent != binding.AgentID {
		return agentmcp.ErrGrantDenied
	}
	if retained {
		switch record.State {
		case attempt.Running, attempt.Snapshotted, attempt.Published, attempt.Durable, attempt.Verifying, attempt.BindReady:
		default:
			return agentmcp.ErrGrantDenied
		}
	} else if record.State != attempt.Running || *record.SessionSettled {
		return agentmcp.ErrGrantDenied
	}
	if err := task.CheckExecutionTx(t.tx, record.Execution); err != nil {
		if errors.Is(err, task.ErrExecutionStopped) {
			return agentmcp.ErrGrantDenied
		}
		return err
	}
	raw, ok, err := t.tx.LoadDocument("tasks")
	if err != nil {
		return err
	}
	if !ok {
		return agentmcp.ErrGrantDenied
	}
	var document struct {
		Tasks map[string]*task.Task `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	work := document.Tasks[scope.TaskID]
	if work == nil || !work.State.Holds() || work.Channel != binding.ConversationID || work.Member != binding.AgentID {
		return agentmcp.ErrGrantDenied
	}
	if binding.TaskID != "" {
		parent := document.Tasks[work.Parent]
		if binding.TaskID != scope.TaskID || record.Kind != attempt.KindDelegate || parent == nil || binding.DelegatedBy != parent.Member || parent.Channel != binding.ConversationID {
			return agentmcp.ErrGrantDenied
		}
	} else if record.Kind != attempt.KindChat || binding.DelegatedBy != "" {
		return agentmcp.ErrGrantDenied
	}
	for _, lease := range record.Leases {
		if lease.Key == "attempt:"+record.ID && lease.Holder == record.ID {
			if err := t.tx.CheckLocalLease(lease); err != nil {
				if errors.Is(err, ledger.ErrStale) || errors.Is(err, ledger.ErrUnknownRegion) {
					return agentmcp.ErrGrantDenied
				}
				return err
			}
			return nil
		}
	}
	return agentmcp.ErrGrantDenied
}

func (t applicationMCPTx) Bind(binding agentmcp.Binding, previous *agentmcp.GrantScope, next agentmcp.GrantScope) error {
	if previous != nil && *previous == next {
		return t.check(binding, next, true)
	}
	if err := t.Authorize(binding, next); err != nil {
		return err
	}
	if previous == nil {
		return nil
	}
	if binding.TaskID != "" || binding.DelegatedBy != "" || previous.AttemptID == next.AttemptID || previous.SessionID != next.SessionID || previous.NodeID != next.NodeID {
		return agentmcp.ErrGrantDenied
	}
	old, err := t.record(*previous)
	if err != nil {
		return err
	}
	if old.Kind != attempt.KindChat || old.Agent != binding.AgentID || old.State != attempt.Bound || old.Unsettled || old.SupersededBy != "" || old.SessionSettled == nil || !*old.SessionSettled {
		return agentmcp.ErrGrantDenied
	}
	if err := task.CheckExecutionTx(t.tx, old.Execution); err != nil {
		if errors.Is(err, task.ErrExecutionStopped) {
			return agentmcp.ErrGrantDenied
		}
		return fmt.Errorf("validate prior MCP execution: %w", err)
	}
	return nil
}
