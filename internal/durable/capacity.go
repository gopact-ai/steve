package durable

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
)

var ErrCapacity = errors.New("service backlog capacity reached; retry the same request after capacity becomes available")

type Limits struct {
	Total     int `json:"total"`
	PerSource int `json:"per_source"`
}

func (l Limits) Effective() Limits {
	if l.Total <= 0 {
		l.Total = 4096
	}
	if l.PerSource <= 0 {
		l.PerSource = 256
	}
	return l
}

// CheckCapacityTx counts a domain's unfinished index inside admission. The
// idempotent-replay check must happen first. Counts do not inspect cold history.
func CheckCapacityTx(tx *ledger.Tx, index, kind, sourcePath, source string, limits Limits, incoming int) error {
	if incoming < 0 {
		incoming = 0
	}
	limits = limits.Effective()
	var total, own int
	if err := tx.QueryRow(`SELECT count(*) FROM bindings WHERE kind=?`, index).Scan(&total); err != nil {
		return err
	}
	if total+incoming > limits.Total {
		return fmt.Errorf("%w: %s global limit %d", ErrCapacity, kind, limits.Total)
	}
	if source == "" {
		return nil
	}
	if !validSourcePath(sourcePath) {
		return errors.New("invalid capacity source path")
	}
	if err := tx.QueryRow(`SELECT count(*) FROM bindings AS q JOIN bindings AS r ON r.kind=? AND r.id=q.id WHERE q.kind=? AND json_extract(r.data,'`+sourcePath+`')=?`, kind, index, source).Scan(&own); err != nil {
		return err
	}
	if own+incoming > limits.PerSource {
		return fmt.Errorf("%w: %s source limit %d", ErrCapacity, kind, limits.PerSource)
	}
	return nil
}

type QueueStats struct {
	Pending int    `json:"pending"`
	Oldest  string `json:"oldest,omitempty"`
}

// CheckStateCapacityTx also covers pre-index records during an upgrade. Only
// unfinished records remain hot; archived terminal pointers do not match.
func CheckStateCapacityTx(tx *ledger.Tx, kind, sourcePath, source string, states []string, limits Limits, incoming int) error {
	if incoming < 0 {
		incoming = 0
	}
	limits = limits.Effective()
	marks := make([]string, len(states))
	args := []any{kind}
	for i, state := range states {
		marks[i] = "?"
		args = append(args, state)
	}
	query := `SELECT count(*) FROM bindings WHERE kind=? AND json_extract(data,'$.state') IN (` + strings.Join(marks, ",") + `)`
	var total, own int
	if err := tx.QueryRow(query, args...).Scan(&total); err != nil {
		return err
	}
	if total+incoming > limits.Total {
		return fmt.Errorf("%w: %s global limit %d", ErrCapacity, kind, limits.Total)
	}
	if source == "" {
		return nil
	}
	if !validSourcePath(sourcePath) {
		return errors.New("invalid capacity source path")
	}
	args = append(args, source)
	if err := tx.QueryRow(query+` AND json_extract(data,'`+sourcePath+`')=?`, args...).Scan(&own); err != nil {
		return err
	}
	if own+incoming > limits.PerSource {
		return fmt.Errorf("%w: %s source limit %d", ErrCapacity, kind, limits.PerSource)
	}
	return nil
}

func validSourcePath(path string) bool {
	return path == "$.source" || path == "$.grant_id" || path == "$.rule.handler"
}

func init() {
	ledger.MustRegisterReadIndex("service_state_source_v1", `CREATE INDEX IF NOT EXISTS service_state_source_v1 ON bindings(kind,json_extract(data,'$.state'),json_extract(data,'$.source'),id)`)
	ledger.MustRegisterReadIndex("service_state_grant_v1", `CREATE INDEX IF NOT EXISTS service_state_grant_v1 ON bindings(kind,json_extract(data,'$.state'),json_extract(data,'$.grant_id'),id)`)
	ledger.MustRegisterReadIndex("service_state_handler_v1", `CREATE INDEX IF NOT EXISTS service_state_handler_v1 ON bindings(kind,json_extract(data,'$.state'),json_extract(data,'$.rule.handler'),id)`)
	ledger.MustRegisterReadIndex("service_state_retention_v1", `CREATE INDEX IF NOT EXISTS service_state_retention_v1 ON bindings(kind,json_extract(data,'$.state'),updated_at,id)`)
	ledger.MustRegisterReadIndex("service_outcome_retention_v1", `CREATE INDEX IF NOT EXISTS service_outcome_retention_v1 ON bindings(kind,json_extract(data,'$.outcome.state'),updated_at,id)`)
}
func QueueStatsTx(r ledger.Reader, index, kind, timePath string) (out QueueStats, err error) {
	err = r.QueryRow(`SELECT count(*),coalesce(min(json_extract(b.data,?)), '') FROM bindings AS q JOIN bindings AS b ON b.kind=? AND b.id=q.id WHERE q.kind=?`, timePath, kind, index).Scan(&out.Pending, &out.Oldest)
	return
}
