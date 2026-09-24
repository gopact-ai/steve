package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func init() {
	MustRegisterReadIndex("commands_pending_kind", `CREATE INDEX IF NOT EXISTS commands_pending_kind ON commands(kind, received_at, id) WHERE acknowledged_by IS NULL`)
}

const pendingCommandsQuery = `SELECT id, kind, actor, received_at, finished_at, result, error
	FROM commands WHERE kind = ? AND acknowledged_by IS NULL ORDER BY received_at, id`

// CommandProof binds a downstream receipt to its accepted input. The command
// carrying this result must itself have finished successfully before it can
// acknowledge that input.
type CommandProof struct {
	CommandID string `json:"command_id"`
	Receipt   string `json:"receipt"`
}

// PendingCommands reads the unacknowledged projection, not completed history.
// Unfinished and failed commands stay visible; neither is permission to retry
// their external effects.
func (l *Ledger) PendingCommands(ctx context.Context, kind string) ([]CommandRecord, error) {
	if kind == "" {
		return nil, errors.New("ledger: a command kind is required")
	}
	rows, err := l.reads.QueryContext(ctx, pendingCommandsQuery, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CommandRecord
	for rows.Next() {
		var r CommandRecord
		var received string
		var finished, raw, cmdErr sql.NullString
		if err := rows.Scan(&r.ID, &r.Kind, &r.Actor, &received, &finished, &raw, &cmdErr); err != nil {
			return nil, err
		}
		r.ReceivedAt, err = time.Parse(time.RFC3339Nano, received)
		if err != nil {
			return nil, fmt.Errorf("command %s received_at: %w", r.ID, err)
		}
		if finished.Valid {
			at, err := time.Parse(time.RFC3339Nano, finished.String)
			if err != nil {
				return nil, fmt.Errorf("command %s finished_at: %w", r.ID, err)
			}
			r.FinishedAt = &at
		}
		r.Result, r.Error = json.RawMessage(raw.String), cmdErr.String
		result = append(result, r)
	}
	return result, rows.Err()
}

// AcknowledgeCommand removes a successfully accepted input from pending only
// with its own successful, identity-matched downstream proof. Input, proof and
// acknowledgement are checked in one transaction. A missing proof returns
// sql.ErrNoRows; an unknown or failed proof never clears pending. History stays.
func (l *Ledger) AcknowledgeCommand(ctx context.Context, id, kind, actor, proofID, proofKind string) error {
	if id == "" || kind == "" || proofID == "" || proofKind == "" || id == proofID {
		return errors.New("ledger: acknowledgement requires an input and distinct proof")
	}
	return l.Update(ctx, func(tx *Tx) error {
		var inputKind, inputActor string
		var finished, inputErr, ack sql.NullString
		if err := tx.QueryRow(`SELECT kind, actor, finished_at, error, acknowledged_by FROM commands WHERE id = ?`, id).
			Scan(&inputKind, &inputActor, &finished, &inputErr, &ack); err != nil {
			return err
		}
		if inputKind != kind || inputActor != actor || ack.Valid && ack.String != proofID {
			return fmt.Errorf("%w: command %s acknowledgement identity changed", ErrConflict, id)
		}
		if !finished.Valid {
			return ErrInFlight
		}
		if _, err := time.Parse(time.RFC3339Nano, finished.String); err != nil || !inputErr.Valid || inputErr.String != "" {
			return fmt.Errorf("%w: command %s was not successfully accepted", ErrConflict, id)
		}
		var actualKind, actualActor string
		var proofFinished, raw, proofErr sql.NullString
		if err := tx.QueryRow(`SELECT kind, actor, finished_at, result, error FROM commands WHERE id = ?`, proofID).
			Scan(&actualKind, &actualActor, &proofFinished, &raw, &proofErr); err != nil {
			return err
		}
		if actualKind != proofKind || actualActor != actor {
			return fmt.Errorf("%w: command %s has another proof identity", ErrConflict, id)
		}
		if !proofFinished.Valid {
			return ErrInFlight
		}
		if _, err := time.Parse(time.RFC3339Nano, proofFinished.String); err != nil || !proofErr.Valid || proofErr.String != "" {
			return fmt.Errorf("%w: command %s has no successful proof", ErrConflict, id)
		}
		var proof CommandProof
		if json.Unmarshal([]byte(raw.String), &proof) != nil || proof.CommandID != id || proof.Receipt == "" {
			return fmt.Errorf("%w: command %s proof is not linked to this input", ErrConflict, id)
		}
		if ack.Valid {
			return nil
		}
		_, err := tx.Exec(`UPDATE commands SET acknowledged_by = ? WHERE id = ? AND acknowledged_by IS NULL`, proofID, id)
		return err
	})
}
