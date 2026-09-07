package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

const relocationSessionKind = "relocation-session-config"

// RelocationSessionConfig is the exact native open payload. MCP launchers can
// contain receiver-owned binding IDs, so a retry must not rebuild them from a
// fresh admission. Like state.Session, its bearer stays in the private ledger.
type RelocationSessionConfig struct {
	MCPServers   []acp.MCPServer `json:"mcp_servers"`
	AgentToken   string          `json:"agent_token,omitempty"`
	Fingerprint  string          `json:"fingerprint"`
	Instructions string          `json:"instructions"`
}

func (s *Service) RelocationSession(ctx context.Context, id string) (RelocationSessionConfig, bool, error) {
	var config RelocationSessionConfig
	ok, err := s.l.GetBinding(ctx, relocationSessionKind, id, &config)
	return config, ok, err
}

// RecordRelocationSession freezes the payload before any native open RPC.
// An existing preparation may reuse it but cannot replace it under the same
// open command after an uncertain RPC response.
func (s *Service) RecordRelocationSession(ctx context.Context, id string, config RelocationSessionConfig) error {
	r, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !PreparingRelocation(r) {
		return errors.New("native open configuration requires an approved preparation")
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("native open configuration exceeds limit")
	}
	_, err = s.l.Transition(ctx, id, string(r.State), string(r.State), "relocation-open", r.Leases, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		var current Record
		if err := json.Unmarshal(op.Data, &current); err != nil {
			return err
		}
		if !PreparingRelocation(current) {
			return errors.New("native preparation phase changed")
		}
		if err := task.CheckExecutionTx(tx, current.Execution); err != nil {
			return err
		}
		var previous string
		if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, relocationSessionKind, id).Scan(&previous); err == nil {
			if previous != string(raw) {
				return errors.New("native open configuration is already fixed")
			}
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return tx.PutBinding(relocationSessionKind, id, config)
	})
	return err
}
