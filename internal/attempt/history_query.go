package attempt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"modernc.org/sqlite"
)

const MaxHistoryLimit = 100

var (
	ErrInvalidHistoryQuery = errors.New("invalid native attempt history query or cursor")
	ErrHistoryChanged      = errors.New("native attempt history changed; refresh from the first page")
)

type HistoryQuery struct {
	TaskID       string `json:"task_id,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type HistoryPage struct {
	Items      []Record `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type nativeHistoryRevision struct {
	Incarnation string `json:"incarnation"`
	Scopes      string `json:"scopes"`
}

type nativeHistoryCursor struct {
	Version      int                   `json:"v"`
	TaskID       string                `json:"task_id,omitempty"`
	Conversation string                `json:"conversation,omitempty"`
	Scope        string                `json:"scope"`
	Revision     nativeHistoryRevision `json:"revision"`
	At           string                `json:"at"`
	ID           string                `json:"id"`
}

const historyTaskExpression = `steve_attempt_history_v1(id,data,'task')`
const historyTimeExpression = `steve_attempt_history_v1(id,data,'at')`
const historyAtTie = ` AND ` + historyTimeExpression + `=? AND id<?`
const historyBeforeTime = ` AND ` + historyTimeExpression + `<?`

func nativeHistoryTime(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// Files choose a completed result by EndedAt, not by task creation order.
// An old task's new result must sort ahead of a newer task's older result.
func nativeHistoryAt(r Record) string {
	if !r.EndedAt.IsZero() {
		return nativeHistoryTime(r.EndedAt)
	}
	return nativeHistoryTime(r.StartedAt)
}

func decodeHistoryRecord(op ledger.Operation) (Record, error) {
	r, err := decode(op)
	if err != nil {
		return Record{}, err
	}
	if op.ID == "" || r.ID != op.ID {
		return Record{}, fmt.Errorf("attempt %s: invalid record identity", op.ID)
	}
	return r, nil
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("steve_attempt_history_v1", 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		id, _ := args[0].(string)
		var raw []byte
		switch v := args[1].(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		}
		r, err := decodeHistoryRecord(ledger.Operation{ID: id, Data: raw})
		if err != nil {
			return nil, nil
		}
		switch args[2] {
		case "task":
			return r.TaskID, nil
		case "at":
			return nativeHistoryAt(r), nil
		default:
			return nil, errors.New("attempt: unknown history index field")
		}
	})
	ledger.MustRegisterReadIndex("operations_task_history", `CREATE INDEX IF NOT EXISTS operations_task_history ON operations(`+historyTaskExpression+`,`+historyTimeExpression+` DESC,id DESC) WHERE kind='attempt'`)
}

func nativeHistorySQL(boundary string) string {
	query := `SELECT id,state,revision,data FROM operations INDEXED BY operations_task_history WHERE kind='attempt' AND ` + historyTaskExpression + `=?` + boundary
	if boundary == historyAtTie {
		return query + ` ORDER BY id DESC LIMIT ?`
	}
	return query + ` ORDER BY ` + historyTimeExpression + ` DESC,id DESC LIMIT ?`
}

func (s *Service) QueryHistory(ctx context.Context, q HistoryQuery) (HistoryPage, error) {
	var page HistoryPage
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		var err error
		page, err = QueryHistoryTx(tx, q)
		return err
	})
	if err != nil {
		return HistoryPage{}, err
	}
	return page, nil
}

// QueryHistoryTx scopes, orders and pages within the caller's SQLite snapshot.
// A task page seeks only its index range. Conversation files are an on-demand
// detail: scoped task IDs plus per-task top(limit+1), merged to a bounded page.
// That cost grows with this conversation's task count, not unrelated history;
// it is deliberately not a base-state or O(live) query.
func QueryHistoryTx(tx *ledger.ReadTx, q HistoryQuery) (HistoryPage, error) {
	if (q.TaskID == "") == (q.Conversation == "") || q.Limit < 0 || q.Limit > MaxHistoryLimit || len(q.Cursor) > 4096 {
		return HistoryPage{}, ErrInvalidHistoryQuery
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	var before nativeHistoryCursor
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(q.Cursor)
		if err != nil {
			return HistoryPage{}, ErrInvalidHistoryQuery
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&before); err != nil {
			return HistoryPage{}, ErrInvalidHistoryQuery
		}
		if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
			return HistoryPage{}, ErrInvalidHistoryQuery
		}
		at, err := time.Parse(time.RFC3339Nano, before.At)
		if err != nil || before.At != nativeHistoryTime(at) || before.Version != 2 || before.ID == "" || before.Scope == "" ||
			before.TaskID != q.TaskID || before.Conversation != q.Conversation {
			return HistoryPage{}, ErrInvalidHistoryQuery
		}
	}
	var revision nativeHistoryRevision
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key='incarnation'`).Scan(&revision.Incarnation); err != nil {
		return HistoryPage{}, err
	}
	if err := ledger.CheckOperationEnvelopesTx(tx, "attempt"); err != nil {
		return HistoryPage{}, err
	}
	if err := checkHistoryRows(tx); err != nil {
		return HistoryPage{}, err
	}
	ids := []string{q.TaskID}
	if q.Conversation != "" {
		var err error
		ids, err = task.ConversationIDsTx(tx, "console", q.Conversation)
		if err != nil {
			return HistoryPage{}, err
		}
	}
	rawScope, _ := json.Marshal(ids)
	digest := sha256.Sum256(rawScope)
	scope := hex.EncodeToString(digest[:])
	// Only point-read one small owner revision per scoped task. Neither
	// payload history nor unrelated ledger writes enter this fingerprint.
	revisions := make([]string, 0, len(ids))
	for _, id := range ids {
		token, err := historyRevisionTx(tx, id)
		if err != nil {
			return HistoryPage{}, err
		}
		revisions = append(revisions, token)
	}
	rawRevisions, _ := json.Marshal(revisions)
	digest = sha256.Sum256(rawRevisions)
	revision.Scopes = hex.EncodeToString(digest[:])
	if q.Cursor != "" {
		if before.Revision != revision || before.Scope != scope {
			return HistoryPage{}, ErrHistoryChanged
		}
		anchor, err := GetTx(tx, before.ID)
		if err != nil || nativeHistoryAt(anchor) != before.At || !slices.Contains(ids, anchor.TaskID) {
			return HistoryPage{}, ErrInvalidHistoryQuery
		}
	}
	top := []Record{}
	for _, taskID := range ids {
		rows, err := readNativeTaskPage(tx, taskID, before, q.Limit+1)
		if err != nil {
			return HistoryPage{}, err
		}
		top = mergeNativeHistory(top, rows, q.Limit+1)
	}
	page := HistoryPage{Items: top}
	if len(top) > q.Limit {
		page.Items = top[:q.Limit]
		last := page.Items[len(page.Items)-1]
		raw, _ := json.Marshal(nativeHistoryCursor{Version: 2, TaskID: q.TaskID, Conversation: q.Conversation, Scope: scope, Revision: revision, At: nativeHistoryAt(last), ID: last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func checkHistoryRows(tx *ledger.ReadTx) error {
	var op ledger.Operation
	var raw []byte
	err := tx.QueryRow(`SELECT id,state,revision,data FROM operations INDEXED BY operations_task_history WHERE kind='attempt' AND `+historyTaskExpression+` IS NULL LIMIT 1`).
		Scan(&op.ID, &op.State, &op.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	op.Data = raw
	_, err = decodeHistoryRecord(op)
	if err == nil {
		err = fmt.Errorf("attempt %s: inconsistent history index", op.ID)
	}
	return err
}

func readNativeTaskPage(tx *ledger.ReadTx, taskID string, before nativeHistoryCursor, limit int) ([]Record, error) {
	read := func(boundary string, args ...any) ([]Record, error) {
		rows, err := tx.Query(nativeHistorySQL(boundary), append([]any{taskID}, args...)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []Record{}
		for rows.Next() {
			var op ledger.Operation
			var raw []byte
			if err := rows.Scan(&op.ID, &op.State, &op.Revision, &raw); err != nil {
				return nil, err
			}
			op.Data = raw
			r, err := decodeHistoryRecord(op)
			if err != nil {
				return nil, err
			}
			if r.TaskID != taskID {
				return nil, fmt.Errorf("attempt %s: inconsistent history scope", op.ID)
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	if before.ID == "" {
		return read("", limit)
	}
	ties, err := read(historyAtTie, before.At, before.ID, limit)
	if err != nil || len(ties) == limit {
		return ties, err
	}
	older, err := read(historyBeforeTime, before.At, limit-len(ties))
	return append(ties, older...), err
}

func nativeHistoryBefore(a, b Record) bool {
	at, bt := nativeHistoryAt(a), nativeHistoryAt(b)
	return at > bt || (at == bt && a.ID > b.ID)
}

// A rolling top K is a two-way merge, retaining only K records regardless of
// the number of tasks in the requested conversation.
func mergeNativeHistory(a, b []Record, limit int) []Record {
	out := make([]Record, 0, min(limit, len(a)+len(b)))
	for len(out) < limit && (len(a) != 0 || len(b) != 0) {
		if len(b) == 0 || (len(a) != 0 && nativeHistoryBefore(a[0], b[0])) {
			out = append(out, a[0])
			a = a[1:]
		} else {
			out = append(out, b[0])
			b = b[1:]
		}
	}
	return out
}
