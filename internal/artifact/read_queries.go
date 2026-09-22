package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

// Writers persist UTC RFC3339Nano. Removing Z gives a chronological text key
// without losing nanoseconds. Trim fractional zeroes as well so equivalent
// UTC spellings tie on the stored ID. Non-UTC timestamps sort first and
// produce an error, never a silently misordered or full-scanned result.
// This expression is also used by the schema indexes.
func recentTimeSQL(value string) string {
	return `CASE WHEN substr(` + value + `, -1) != 'Z' THEN '~'
		WHEN instr(` + value + `, '.') > 0 THEN rtrim(rtrim(rtrim(` + value + `, 'Z'), '0'), '.')
		ELSE rtrim(` + value + `, 'Z') END`
}

var recentLandingTimeSQL = `CASE WHEN json_valid(data) THEN CASE WHEN json_type(data) IN ('object', 'null') THEN ` + recentTimeSQL(
	`COALESCE(NULLIF(json_extract(data, '$.ended_at'), '0001-01-01T00:00:00Z'), json_extract(data, '$.started_at'), '0001-01-01T00:00:00Z')`) + ` ELSE '~' END ELSE '~' END`

var recentFactTimeSQL = `CASE WHEN json_valid(data) THEN CASE WHEN json_type(data) IN ('object', 'null') THEN ` + recentTimeSQL(
	`COALESCE(json_extract(data, '$.at'), '0001-01-01T00:00:00Z')`) + ` ELSE '~' END ELSE '~' END`

var recentLandingsQuery = `SELECT id, state, data, ` + recentLandingTimeSQL + ` FROM operations INDEXED BY operations_recent_landings
	WHERE kind = 'landing' ORDER BY ` + recentLandingTimeSQL + ` DESC, id DESC LIMIT 20`

var recentFactsQuery = `SELECT id, data, ` + recentFactTimeSQL + ` FROM bindings INDEXED BY bindings_recent_artifact_facts
	WHERE kind = ? AND kind IN ('attestation', 'replica')
	ORDER BY ` + recentFactTimeSQL + ` DESC, id DESC LIMIT ?`

func init() {
	ledger.MustRegisterReadIndex("operations_recent_landings", `CREATE INDEX IF NOT EXISTS operations_recent_landings ON operations (`+recentLandingTimeSQL+` DESC, id DESC) WHERE kind = 'landing'`)
	ledger.MustRegisterReadIndex("bindings_recent_artifact_facts", `CREATE INDEX IF NOT EXISTS bindings_recent_artifact_facts ON bindings (kind, `+recentFactTimeSQL+` DESC, id DESC) WHERE kind IN ('attestation', 'replica')`)
}

// RecentLandings reads one global window of at most 20 candidates, including
// retained history whose project is no longer active. Bad candidates return
// an error and the valid subset; they are not replaced by an unbounded search.
// Pending conflicts are deliberately read separately through AllStuck.
func (s *Store) RecentLandings(ctx context.Context) ([]Landing, error) {
	rows, err := s.ledger.DB().QueryContext(ctx, recentLandingsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Landing
	var failures []error
	for rows.Next() {
		var id, state, timeKey string
		var raw []byte
		if err := rows.Scan(&id, &state, &raw, &timeKey); err != nil {
			return out, errors.Join(append(failures, err)...)
		}
		var land Landing
		if err := json.Unmarshal(raw, &land); err != nil {
			failures = append(failures, fmt.Errorf("read landing %s: %w", id, err))
			continue
		}
		if timeKey == "~" {
			failures = append(failures, fmt.Errorf("read landing %s: recent time must be UTC RFC3339", id))
			continue
		}
		land.State = state
		out = append(out, land)
	}
	return out, errors.Join(append(failures, rows.Err())...)
}

// RecentAttestations reads at most 30 recent candidates, oldest first for the
// facts view. Its error describes the requested window, not a history audit.
func (s *Store) RecentAttestations(ctx context.Context) ([]Attestation, error) {
	rows, err := s.ledger.DB().QueryContext(ctx, recentFactsQuery, attestationKind, 30)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attestation
	var failures []error
	for rows.Next() {
		var id, timeKey string
		var raw []byte
		if err := rows.Scan(&id, &raw, &timeKey); err != nil {
			failures = append(failures, err)
			break
		}
		var a Attestation
		if err := json.Unmarshal(raw, &a); err != nil {
			failures = append(failures, fmt.Errorf("read attestation %s: %w", id, err))
			continue
		}
		if timeKey == "~" {
			failures = append(failures, fmt.Errorf("read attestation %s: recent time must be UTC RFC3339", id))
			continue
		}
		out = append(out, a)
	}
	slices.Reverse(out)
	return out, errors.Join(append(failures, rows.Err())...)
}

// RecentReplicas reads at most 40 recent candidates, oldest first. Operational
// replica selection still uses Replicas and its complete-list error contract.
func (s *Store) RecentReplicas(ctx context.Context) ([]Replica, error) {
	rows, err := s.ledger.DB().QueryContext(ctx, recentFactsQuery, replicaKind, 40)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Replica
	var failures []error
	for rows.Next() {
		var id, timeKey string
		var raw []byte
		if err := rows.Scan(&id, &raw, &timeKey); err != nil {
			failures = append(failures, err)
			break
		}
		var r Replica
		if err := json.Unmarshal(raw, &r); err != nil {
			failures = append(failures, fmt.Errorf("read replica %s: %w", id, err))
			continue
		}
		if timeKey == "~" {
			failures = append(failures, fmt.Errorf("read replica %s: recent time must be UTC RFC3339", id))
			continue
		}
		out = append(out, r)
	}
	slices.Reverse(out)
	return out, errors.Join(append(failures, rows.Err())...)
}
