package task

import (
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
	"modernc.org/sqlite"
)

const conversationScopeExpression = `steve_task_conversation_v1(id,data)`
const conversationScopeFrom = ` FROM bindings INDEXED BY task_conversation_headers WHERE kind='task' AND ` + conversationScopeExpression

func conversationScopeKey(transport, conversation string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(transport)) + "/" + base64.RawURLEncoding.EncodeToString([]byte(conversation))
}

func decodeScopeHeader(id string, raw []byte) (taskHead, error) {
	var head *taskHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return taskHead{}, fmt.Errorf("task: decode header %s: %w", id, err)
	}
	if head == nil || id == "" || head.ID != id || head.AttemptCount < 0 || len(head.Attempts) != 0 {
		return taskHead{}, fmt.Errorf("task: invalid header %s", id)
	}
	return *head, nil
}

func init() {
	for _, field := range []string{"conversation", "parent"} {
		sqlite.MustRegisterDeterministicScalarFunction("steve_task_"+field+"_v1", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			id, _ := args[0].(string)
			var raw []byte
			switch v := args[1].(type) {
			case string:
				raw = []byte(v)
			case []byte:
				raw = v
			}
			head, err := decodeScopeHeader(id, raw)
			if err != nil {
				return nil, nil
			}
			if field == "parent" {
				return head.Parent, nil
			}
			return conversationScopeKey(head.Transport, head.Channel), nil
		})
	}
	ledger.MustRegisterReadIndex("task_conversation_headers", `CREATE INDEX IF NOT EXISTS task_conversation_headers ON bindings(`+conversationScopeExpression+`,id) WHERE kind='task'`)
	ledger.MustRegisterReadIndex("task_parent_headers", `CREATE INDEX IF NOT EXISTS task_parent_headers ON bindings(steve_task_parent_v1(id,data),id) WHERE kind='task'`)
}

// ConversationIDsTx is for an explicitly requested conversation detail, not
// the base state. Its cost is proportional to that conversation's task headers,
// and their descendants, never their accounting histories or unrelated valid conversations. The
// caller keeps this boundary open while querying attempts for those tasks.
func ConversationIDsTx(tx *ledger.ReadTx, transport, conversation string) ([]string, error) {
	var badID string
	var raw []byte
	err := tx.QueryRow(`SELECT id,data`+conversationScopeFrom+` IS NULL LIMIT 1`).Scan(&badID, &raw)
	if err == nil {
		_, err := decodeScopeHeader(badID, raw)
		if err == nil {
			err = fmt.Errorf("task: inconsistent conversation index for %s", badID)
		}
		return nil, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := tx.Query(`SELECT * FROM (WITH RECURSIVE scoped(id) AS (
        SELECT id`+conversationScopeFrom+`=?
        UNION
        SELECT b.id FROM bindings b INDEXED BY task_parent_headers JOIN scoped s
          ON steve_task_parent_v1(b.id,b.data)=s.id WHERE b.kind='task'
    ) SELECT b.id,steve_task_parent_v1(b.id,b.data),p.id
      FROM scoped s JOIN bindings b ON b.kind='task' AND b.id=s.id
      LEFT JOIN bindings p ON p.kind='task' AND p.id=steve_task_parent_v1(b.id,b.data)
      ORDER BY b.id)`, conversationScopeKey(transport, conversation))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	parents := map[string]string{}
	for rows.Next() {
		var id, parent string
		var parentID sql.NullString
		if err := rows.Scan(&id, &parent, &parentID); err != nil {
			return nil, err
		}
		if parent != "" && !parentID.Valid {
			return nil, fmt.Errorf("task: %s has missing parent %s", id, parent)
		}
		parents[id] = parent
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// UNION bounds traversal even for corrupt cycles; reject rather than
	// presenting such a closure as a healthy task tree.
	checked := map[string]bool{}
	for _, id := range ids {
		path := map[string]bool{}
		for current := id; current != "" && !checked[current]; current = parents[current] {
			if path[current] {
				return nil, fmt.Errorf("task: conversation ancestry cycle at %s", current)
			}
			path[current] = true
		}
		for current := range path {
			checked[current] = true
		}
	}
	return ids, nil
}
