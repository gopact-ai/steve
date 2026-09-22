package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
)

func attemptOwnerBook(t *testing.T) (*Gateway, *ledger.Ledger) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	g := New(&recoveryProbe{})
	g.SetRecoveryLedger(book)
	raw, err := json.Marshal(gatewayInput{Message: feishu.InboundMessage{ConversationID: "conversation", MessageID: "input-message", SenderOpenID: "owner"}, ExpectedTask: "original-task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := book.RecordCommand(t.Context(), "input", gatewayInputKind, "owner", raw); err != nil {
		t.Fatal(err)
	}
	if err := rememberRecoveryAttempt(t.Context(), book, "input", "owner", turn.Request{ConversationID: "conversation", MessageID: "input-message", ExpectedTask: "original-task"}, "original-task", "original-attempt"); err != nil {
		t.Fatal(err)
	}
	return g, book
}

func ownsOriginal(t *testing.T, g *Gateway, book *ledger.Ledger) (bool, error) {
	t.Helper()
	return g.OwnsAttempt(t.Context(), book, "original-attempt", "original-task", "conversation", "input-message", "owner")
}

func TestGatewayAttemptOwnerUsesGoJSONSemanticsAndFailsClosed(t *testing.T) {
	const fields = `"input_id":"input","task_id":"original-task","conversation":"conversation","message_id":"input-message",`
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"canonical", `{` + fields + `"attempt_id":"original-attempt"}`, true},
		{"case-folded", `{` + fields + `"ATTEMPT_ID":"original-attempt"}`, true},
		{"escaped-key", `{` + fields + `"attempt_\u0069d":"original-attempt"}`, true},
		{"last-duplicate-wins", `{` + fields + `"attempt_id":"other","attempt_id":"original-attempt"}`, true},
		{"null-preserves-prior-string", `{` + fields + `"attempt_id":"original-attempt","attempt_id":null}`, true},
		{"typed-error-is-not-overwritten", `{` + fields + `"attempt_id":42,"attempt_id":"original-attempt"}`, false},
		{"null-only", `{` + fields + `"attempt_id":null}`, false},
		{"missing-task", `{"input_id":"input","conversation":"conversation","message_id":"input-message","attempt_id":"original-attempt"}`, false},
		{"malformed-unrelated-owner", `{"attempt_id":"other"`, false},
		{"array", `[]`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			g, book := attemptOwnerBook(t)
			if _, err := book.DB().Exec(`UPDATE commands SET result=? WHERE id='input/attempt'`, test.raw); err != nil {
				t.Fatal(err)
			}
			owned, err := ownsOriginal(t, g, book)
			if test.valid && (!owned || err != nil) {
				t.Fatalf("valid original owner not found: owned=%v err=%v", owned, err)
			}
			if !test.valid && (owned || err == nil) {
				t.Fatalf("corrupt proof was treated as absence or authority: owned=%v err=%v", owned, err)
			}
		})
	}
}

func TestGatewayAttemptOwnerRequiresOriginalSuccessfulInput(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE commands SET error=NULL WHERE id='input/attempt'`,
		`UPDATE commands SET error=NULL WHERE id='input'`,
		`UPDATE commands SET finished_at=NULL WHERE id='input/attempt'`,
		`UPDATE commands SET actor='other' WHERE id='input/attempt'`,
		`UPDATE commands SET result=json_set(result,'$.message.MessageID','other') WHERE id='input'`,
		`UPDATE commands SET result=json_set(result,'$.message.ConversationID','other') WHERE id='input'`,
		`UPDATE commands SET result=json_set(result,'$.message.SenderOpenID','other') WHERE id='input'`,
		`UPDATE commands SET result=json_set(result,'$.expected_task','other') WHERE id='input'`,
		`UPDATE commands SET result='{}' WHERE id='input'`,
		`DELETE FROM commands WHERE id='input'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			g, book := attemptOwnerBook(t)
			if _, err := book.DB().Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if owned, err := ownsOriginal(t, g, book); owned || err == nil {
				t.Fatalf("unlinked/corrupt original acceptance suppressed result: owned=%v err=%v", owned, err)
			}
		})
	}
}

func TestGatewayAttemptOwnerRejectsAnotherResultIdentity(t *testing.T) {
	for _, field := range []string{"task", "conversation", "message", "requester"} {
		t.Run(field, func(t *testing.T) {
			g, book := attemptOwnerBook(t)
			r := revivalFixture()
			r.TaskID, r.MessageID = "original-task", "input-message"
			switch field {
			case "task":
				r.TaskID = "other"
			case "conversation":
				r.ConversationID = "other"
			case "message":
				r.MessageID = "other"
			case "requester":
				r.Requester = "other"
			}
			if err := g.QueueRecovery(t.Context(), book, "task-result/original-attempt", r, "original-attempt"); err == nil {
				t.Fatal("another identity was treated as already owned")
			}
			if _, exists, err := book.CommandReceipt(t.Context(), "task-result/original-attempt"); err != nil || exists {
				t.Fatalf("conflicting identity created executable result intent: exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestGatewayAttemptOwnerRejectsMultipleAcceptedInputs(t *testing.T) {
	g, book := attemptOwnerBook(t)
	input, _, err := book.CommandReceipt(t.Context(), "input")
	if err != nil {
		t.Fatal(err)
	}
	if err := book.RecordCommand(t.Context(), "second", gatewayInputKind, "owner", input.Result); err != nil {
		t.Fatal(err)
	}
	if err := rememberRecoveryAttempt(t.Context(), book, "second", "owner", turn.Request{ConversationID: "conversation", MessageID: "input-message"}, "original-task", "original-attempt"); err != nil {
		t.Fatal(err)
	}
	if owned, err := ownsOriginal(t, g, book); owned || err == nil {
		t.Fatalf("ambiguous delivery owner hidden: owned=%v err=%v", owned, err)
	}
}

func checkAttemptOwnerPlan(t *testing.T, book *ledger.Ledger) {
	t.Helper()
	var path string
	if err := book.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	// The production diagnostic port intentionally exposes SELECT only.
	// Use a separate read-only test connection for SQLite's EXPLAIN opcode.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+attemptInputQuery, "original-attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH commands USING INDEX commands_gateway_attempt") || strings.Contains(joined, "SCAN ") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("ownership lookup is not an indexed point read:\n%s", joined)
	}
	t.Log(joined)
}

func TestGatewayAttemptOwnerIndexReadsFixedCandidatesAndRebuildsOnRestore(t *testing.T) {
	g, book := attemptOwnerBook(t)
	// Invalid dates would make a history decoder fail, but do not affect
	// ownership of the one requested attempt. All 10k are completed history.
	if _, err := book.DB().Exec(`WITH RECURSIVE n(seq) AS
		(VALUES(1) UNION ALL SELECT seq+1 FROM n WHERE seq<10000)
		INSERT INTO commands(id,kind,actor,received_at,finished_at,result,error,acknowledged_by)
		SELECT 'old-'||seq,'gateway-input-attempt','owner','bad-date','bad-date',
			json_object('input_id','old-'||seq,'task_id','old','attempt_id','old-'||seq,
				'conversation','old','message_id','old'),'','done' FROM n`); err != nil {
		t.Fatal(err)
	}
	verify := func(book *ledger.Ledger) {
		checkAttemptOwnerPlan(t, book)
		var count int
		if err := book.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM (`+attemptInputQuery+`)`, "original-attempt").Scan(&count); err != nil || count != 1 {
			t.Fatalf("candidate reads grew with history: count=%d err=%v", count, err)
		}
		if owned, err := ownsOriginal(t, g, book); err != nil || !owned {
			t.Fatalf("indexed owner not found: owned=%v err=%v", owned, err)
		}
	}
	verify(book)
	if _, err := book.DB().Exec(`DROP INDEX commands_gateway_attempt`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	target, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	verify(target)
}

func TestGatewayResumeAttemptOwnerRequiresItsNoticeAnchor(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			g, book := attemptOwnerBook(t)
			// Replace the ordinary input with a recovery acceptance that
			// legitimately changes anchor only through its notice receipt.
			if _, err := book.DB().Exec(`DELETE FROM commands WHERE id IN ('input','input/attempt')`); err != nil {
				t.Fatal(err)
			}
			r := revivalFixture()
			r.TaskID = "original-task"
			if err := g.QueueRecovery(t.Context(), book, "input", r, ""); err != nil {
				t.Fatal(err)
			}
			if err := book.RecordCommand(context.Background(), "input/notice", "gateway-recovery-notice", "owner", json.RawMessage(`"input-message"`)); err != nil {
				t.Fatal(err)
			}
			if err := rememberRecoveryAttempt(t.Context(), book, "input", "owner", turn.Request{ConversationID: "conversation", MessageID: "input-message"}, "original-task", "original-attempt"); err != nil {
				t.Fatal(err)
			}
			if broken {
				if _, err := book.DB().Exec(`UPDATE commands SET result='"other-anchor"' WHERE id='input/notice'`); err != nil {
					t.Fatal(err)
				}
			}
			owned, err := ownsOriginal(t, g, book)
			if broken && (owned || err == nil) || !broken && (!owned || err != nil) {
				t.Fatalf("notice association not enforced: owned=%v err=%v", owned, err)
			}
		})
	}
}
