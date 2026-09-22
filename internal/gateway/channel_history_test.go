package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
)

func historyBook(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{Now: func() time.Time { return time.Date(2026, 9, 21, 1, 2, 3, 1, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	return book
}
func historyRecord(t *testing.T, book *ledger.Ledger, id, kind string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.RecordCommand(t.Context(), id, kind, "owner", raw); err != nil {
		t.Fatal(err)
	}
}
func historyInput(t *testing.T, book *ledger.Ledger, id, conversation, text string) {
	t.Helper()
	historyRecord(t, book, id, gatewayInputKind, gatewayInput{Message: feishu.InboundMessage{ConversationID: conversation, ChatID: "group", MessageID: id, SenderOpenID: "owner", Text: text, Mentioned: true}})
}
func historyOutput(t *testing.T, book *ledger.Ledger, id, body string) {
	t.Helper()
	historyRecord(t, book, id+"/dispatch", "gateway-input-dispatch", recoveredOutput{Result: turn.Result{Text: body, Attempt: "attempt-" + id}})
}
func historyProof(t *testing.T, book *ledger.Ledger, id string) {
	t.Helper()
	historyRecord(t, book, id+"/reply", "gateway-input-reply", ledger.CommandProof{CommandID: id, Receipt: "external-" + id})
}

func TestChannelHistoryCompletedRunningAndReadOnly(t *testing.T) {
	book := historyBook(t)
	historyInput(t, book, "a", "chat", "first\nsecond line")
	historyOutput(t, book, "a", "saved answer")
	historyProof(t, book, "a")
	historyInput(t, book, "b", "chat", "still running")
	var before int
	if err := book.DB().QueryRow(`SELECT count(*) FROM commands`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	h := NewChannelHistory(book)
	page, err := h.List(t.Context(), "", 10)
	if err != nil || len(page.Conversations) != 1 {
		t.Fatalf("list: %+v %v", page, err)
	}
	c := page.Conversations[0]
	if c.ID != "chat" || c.Transport != "feishu" || !c.ReadOnly || c.Title != "first" || c.Count != 3 || c.LastAt.IsZero() {
		t.Fatalf("summary: %+v", c)
	}
	got, err := h.Read(t.Context(), "chat", "", 1)
	if err != nil || len(got.Replies) != 1 || got.NextCursor == "" || got.Replies[0].Input != "still running" {
		t.Fatalf("latest page: %+v %v", got, err)
	}
	next, err := h.Read(t.Context(), "chat", got.NextCursor, 1)
	if err != nil || len(next.Replies) != 2 || next.NextCursor != "" {
		t.Fatalf("older page: %+v %v", next, err)
	}
	if next.Replies[0].Kind != "sent" || next.Replies[0].Input != "first\nsecond line" || next.Replies[1].Text != "saved answer" || next.Replies[1].Delivery != "confirmed" {
		t.Fatalf("replies: %+v", next.Replies)
	}

	var after int
	if err := book.DB().QueryRow(`SELECT count(*) FROM commands`).Scan(&after); err != nil || after != before {
		t.Fatalf("read wrote commands: %d %d %v", before, after, err)
	}
}

func TestChannelHistoryTopicRouteAndInvalidProofIsolation(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "bad"}[bad], func(t *testing.T) {
			book := historyBook(t)
			historyInput(t, book, "a", "group", "/topic original task")
			historyOutput(t, book, "a", "thread private answer")
			proof := topicReceipt{InputID: "a", Anchor: "anchor", Thread: "thread"}
			if bad {
				proof.InputID = "other"
			}
			historyRecord(t, book, "a/topic", "gateway-input-topic", proof)
			historyInput(t, book, "b", "thread-2", "another topic")
			h := NewChannelHistory(book)
			exists, err := h.Contains(t.Context(), "thread")
			if err != nil || exists == bad {
				t.Fatalf("contains %v %v", exists, err)
			}
			if !bad {
				got, err := h.Read(t.Context(), "thread", "", 10)
				if err != nil || len(got.Replies) != 2 || got.Replies[0].Input != "/topic original task" {
					t.Fatalf("route: %+v %v", got, err)
				}
				exists, err = h.Contains(t.Context(), "group")
				if err != nil || exists {
					t.Fatalf("redirected input in group: %v %v", exists, err)
				}
			} else {
				got, err := h.Read(t.Context(), "group", "", 10)
				if err != nil || len(got.Replies) != 1 {
					t.Fatalf("bad route leaked output: %+v %v", got, err)
				}
			}
		})
	}
}

func TestChannelHistoryOpaqueIDsCursorsAndReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	book, err := ledger.Open(dir, ledger.Options{Now: func() time.Time { return time.Unix(123, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"console:opaque", "x' OR 1=1 --", "a/../b?%00\x00", "!invalid", "群/话题"}
	for _, id := range ids {
		for _, key := range []string{"a", "b", "c"} {
			historyInput(t, book, key+id, id, key)
		}
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	h := NewChannelHistory(book)
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := h.List(t.Context(), cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Conversations {
			if seen[c.ID] {
				t.Fatalf("duplicate %q", c.ID)
			}
			seen[c.ID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != len(ids) {
		t.Fatalf("lost identities: %#v", seen)
	}
	first, err := h.Read(t.Context(), ids[0], "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Read(t.Context(), ids[1], first.NextCursor, 1); !errors.Is(err, consoleapi.ErrChannelHistoryCursor) {
		t.Fatalf("cross conversation cursor: %v", err)
	}
	if _, err = h.List(t.Context(), first.NextCursor, 1); !errors.Is(err, consoleapi.ErrChannelHistoryCursor) {
		t.Fatalf("cross endpoint cursor: %v", err)
	}
	cursor = ""
	var texts []string
	for {
		p, err := h.Read(t.Context(), ids[0], cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Replies {
			texts = append(texts, r.Input)
		}
		cursor = p.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(texts) != 3 || texts[0] != "c" || texts[1] != "b" || texts[2] != "a" {
		t.Fatalf("equal timestamp pagination: %q", texts)
	}
	if _, err = h.Read(t.Context(), "missing", "", 10); !errors.Is(err, consoleapi.ErrChannelConversationNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestChannelHistoryUnknownSuppressedRecoveryAndNoAttemptOutput(t *testing.T) {
	book := historyBook(t)
	historyInput(t, book, "a", "chat", "unknown delivery")
	historyOutput(t, book, "a", "retained answer")
	historyInput(t, book, "b", "chat", "listen")
	historyOutput(t, book, "b", "")
	historyRecord(t, book, "b/suppressed", "gateway-input-suppressed", ledger.CommandProof{CommandID: "b", Receipt: "unmentioned-empty-output"})
	r := recoveryInput{Revival: Revival{ConversationID: "chat", MessageID: "anchor", Requester: "owner", TaskID: "task", Member: "agent"}, Prompt: "resume prompt", Notice: "resume notice", Attempt: "private-attempt"}
	historyRecord(t, book, "c", recoveryInputKind, r)
	historyRecord(t, book, "c/reply", "gateway-recovery-reply", ledger.CommandProof{CommandID: "c", Receipt: "confirmed"})
	historyRecord(t, book, "c/attempt", "gateway-input-attempt", map[string]string{"output": "NEVER DISCLOSE"})
	h := NewChannelHistory(book)
	got, err := h.Read(t.Context(), "chat", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Replies) != 6 {
		t.Fatalf("replies: %+v", got.Replies)
	}
	if got.Replies[1].Delivery != "unconfirmed" || got.Replies[3].Delivery != "suppressed" || got.Replies[5].Delivery != "confirmed" || got.Replies[5].Text != "" || got.Replies[4].Input != "resume prompt" || !got.Replies[4].Relayed {
		t.Fatalf("delivery/body: %+v", got.Replies)
	}
}

func TestChannelHistoryIndexedLookupAndDirectory(t *testing.T) {
	book := historyBook(t)
	historyInput(t, book, "input", "conversation", "title")
	historyOutput(t, book, "input", "answer")
	// Unrelated rows must never be deserialized by a scoped read.
	if _, err := book.DB().Exec(`WITH RECURSIVE n(seq) AS (VALUES(1) UNION ALL SELECT seq+1 FROM n WHERE seq<1000)
 INSERT INTO commands(id,kind,actor,received_at,finished_at,result,error)
 SELECT 'old-'||seq,'gateway-input','owner','bad-date','bad-date',
 json_object('message',json_object('MessageID','old-'||seq,'SenderOpenID','owner','ConversationID','z-other','Text','unrelated')),'' FROM n`); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := book.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	queries := []struct {
		name, sql string
		args      []any
	}{
		{"lookup", `SELECT id,at FROM (` + channelTurnsSQL("") + `) ORDER BY at DESC,id DESC LIMIT ?`, []any{"conversation", "conversation", 2}},
		{"same-time", `SELECT id,at FROM (` + channelTurnsSQL(` AND rtrim(i.received_at,'Z')=? AND i.id<?`) + `) ORDER BY at DESC,id DESC LIMIT ?`, []any{"conversation", "2026-09-21T01:02:03.000000001", "z", "conversation", "2026-09-21T01:02:03.000000001", "z", 2}},
		{"summary", channelSummarySQL(), []any{"conversation", "conversation"}},
		{"directory", channelCandidatesSQL(), []any{"", 2, "", 2, 2}},
	}
	for _, q := range queries {
		t.Run(q.name, func(t *testing.T) {
			rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+q.sql, q.args...)
			if err != nil {
				t.Fatal(err)
			}
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, "\n")
			t.Log(plan)
			expected := "SEARCH i USING INDEX commands_channel_input"
			if q.name == "directory" {
				expected = "SEARCH commands USING INDEX commands_channel_input"
			}
			if !strings.Contains(plan, expected) || strings.Contains(plan, "SCAN commands") || strings.Contains(plan, "SCAN i ") || strings.Contains(plan, "SCAN t ") {
				t.Fatalf("unbounded base-table lookup:\n%s", plan)
			}
			// A Function opcode here would decode historical bodies on each read.
			rows, err = db.QueryContext(t.Context(), "EXPLAIN "+q.sql, q.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var addr, p1, p2, p3, p5 int
				var op string
				var p4, comment sql.NullString
				if err := rows.Scan(&addr, &op, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
					t.Fatal(err)
				}
				if op == "Function" && strings.Contains(p4.String, "steve_channel_fact") {
					t.Fatalf("read re-decodes history instead of index facts: %s", p4.String)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := NewChannelHistory(book).Read(t.Context(), "conversation", "", 1); err != nil {
		t.Fatal(err)
	}
}

func TestChannelHistoryGoJSONSemanticsAndInvalidAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"case", `{"MESSAGE":{"conversationid":"opaque","MESSAGEID":"m","senderopenid":"owner","text":"prompt"}}`, true},
		{"escaped", `{"message":{"Conversation\u0049D":"opaque","MessageID":"m","SenderOpenID":"owner"}}`, true},
		{"duplicate", `{"message":{"ConversationID":"other","ConversationID":"opaque","MessageID":"m","SenderOpenID":"owner"}}`, true},
		{"null-preserves", `{"message":{"ConversationID":"opaque","ConversationID":null,"MessageID":"m","SenderOpenID":"owner"}}`, true},
		{"typed-error", `{"message":{"ConversationID":42,"ConversationID":"opaque","MessageID":"m","SenderOpenID":"owner"}}`, false},
		{"actor", `{"message":{"ConversationID":"opaque","MessageID":"m","SenderOpenID":"other"}}`, false},
		{"missing-message", `{"message":{"ConversationID":"opaque","SenderOpenID":"owner"}}`, false},
		{"null", `null`, false},
		{"array", `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book := historyBook(t)
			if err := book.RecordCommand(t.Context(), "input", gatewayInputKind, "owner", json.RawMessage(tc.raw)); err != nil {
				t.Fatal(err)
			}
			h := NewChannelHistory(book)
			found, err := h.Contains(t.Context(), "opaque")
			if err != nil || found != tc.valid {
				t.Fatalf("Go decode mismatch: found=%v err=%v", found, err)
			}
			page, err := h.List(t.Context(), "", 200)
			if err != nil || len(page.Conversations) != map[bool]int{true: 1, false: 0}[tc.valid] {
				t.Fatalf("directory: %+v %v", page, err)
			}
			found, err = h.Contains(t.Context(), "other")
			if err != nil || found {
				t.Fatalf("first duplicate leaked: %v %v", found, err)
			}
		})
	}
	for _, mutation := range []string{`UPDATE commands SET finished_at=NULL WHERE id='input'`, `UPDATE commands SET error=NULL WHERE id='input'`, `UPDATE commands SET error='failed' WHERE id='input'`} {
		t.Run(mutation, func(t *testing.T) {
			book := historyBook(t)
			historyInput(t, book, "input", "chat", "prompt")
			if _, err := book.DB().Exec(mutation); err != nil {
				t.Fatal(err)
			}
			found, err := NewChannelHistory(book).Contains(t.Context(), "chat")
			if err != nil || found {
				t.Fatalf("unaccepted input visible: %v %v", found, err)
			}
		})
	}
}

func TestChannelHistoryRejectsUnrelatedRouteAndDeliveryProofs(t *testing.T) {
	for _, mutation := range []string{
		`DELETE FROM commands WHERE id='input/topic'`,
		`UPDATE commands SET actor='other' WHERE id='input/topic'`,
		`UPDATE commands SET kind='other' WHERE id='input/topic'`,
		`UPDATE commands SET finished_at=NULL WHERE id='input/topic'`,
		`UPDATE commands SET error='unknown' WHERE id='input/topic'`,
		`UPDATE commands SET error=NULL WHERE id='input/topic'`,
		`UPDATE commands SET result='{"input_id":"input","anchor":"","thread":"thread"}' WHERE id='input/topic'`,
		`UPDATE commands SET result='{"input_id":"input","anchor":"anchor","thread":"group"}' WHERE id='input/topic'`,
		`UPDATE commands SET result='{"input_id":"other","anchor":"anchor","thread":"thread"}' WHERE id='input/topic'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			book := historyBook(t)
			historyInput(t, book, "input", "group", "/topic task")
			historyOutput(t, book, "input", "private result")
			historyProof(t, book, "input")
			historyRecord(t, book, "input/topic", "gateway-input-topic", topicReceipt{InputID: "input", Anchor: "anchor", Thread: "thread"})
			if _, err := book.DB().Exec(mutation); err != nil {
				t.Fatal(err)
			}
			h := NewChannelHistory(book)
			found, err := h.Contains(t.Context(), "thread")
			if err != nil || found {
				t.Fatalf("bad route owns thread: %v %v", found, err)
			}
			got, err := h.Read(t.Context(), "group", "", 200)
			if strings.Contains(mutation, "error=NULL") && err != nil && len(got.Replies) == 0 {
				return
			}
			if err != nil || len(got.Replies) != 1 || got.Conversation.Count != 1 {
				t.Fatalf("private output leaked to group: %+v %v", got, err)
			}
		})
	}
	for _, mutation := range []string{
		`UPDATE commands SET actor='other' WHERE id='input/reply'`,
		`UPDATE commands SET kind='gateway-recovery-reply' WHERE id='input/reply'`,
		`UPDATE commands SET finished_at=NULL WHERE id='input/reply'`,
		`UPDATE commands SET error='unknown' WHERE id='input/reply'`,
		`UPDATE commands SET error=NULL WHERE id='input/reply'`,
		`UPDATE commands SET result='{"command_id":"other","receipt":"ok"}' WHERE id='input/reply'`,
		`UPDATE commands SET result='{"command_id":"input","receipt":""}' WHERE id='input/reply'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			book := historyBook(t)
			historyInput(t, book, "input", "chat", "prompt")
			historyOutput(t, book, "input", "saved")
			historyProof(t, book, "input")
			if _, err := book.DB().Exec(mutation); err != nil {
				t.Fatal(err)
			}
			got, err := NewChannelHistory(book).Read(t.Context(), "chat", "", 200)
			if strings.Contains(mutation, "error=NULL") && err != nil && len(got.Replies) == 0 {
				return
			}
			if err != nil || len(got.Replies) != 2 || got.Replies[1].Delivery != "unconfirmed" {
				t.Fatalf("invalid confirmation: %+v %v", got, err)
			}
		})
	}
}

func TestChannelHistoryActualIngressOwnersAndRecovery(t *testing.T) {
	for _, mode := range []string{"ordinary", "topic", "recovery", "suppressed"} {
		t.Run(mode, func(t *testing.T) {
			book := historyBook(t)
			g := New(&durableInputProbe{})
			g.BindChannel(&ingressTopicChannel{})
			g.SetRecoveryLedger(book)
			conversation := "conversation"
			switch mode {
			case "recovery":
				g = New(&recoveryProbe{})
				g.BindChannel(&recoveryChannel{})
				if err := g.QueueRecovery(t.Context(), book, "recovery", revivalFixture(), ""); err != nil {
					t.Fatal(err)
				}
				if err := g.RecoverQueued(t.Context(), book, &recoveryProbe{}, func(string, string) error { return nil }); err != nil {
					t.Fatal(err)
				}
			default:
				msg := inboundFixture()
				if mode == "topic" {
					msg.ConversationID, msg.Text = msg.ChatID, "/t original"
					conversation = "topic-thread"
				}
				if mode == "suppressed" {
					g = New(suppressedReceiptProbe{})
					g.BindChannel(&recoveryChannel{})
					g.SetRecoveryLedger(book)
					msg.ChatType, msg.Mentioned = protocol.ChatGroup, false
				}
				var workers recoveryTestWorkers
				g.SetIngressLifetime(t.Context(), &workers)
				if err := g.HandleMessage(msg); err != nil {
					t.Fatal(err)
				}
				workers.Wait()
			}
			got, err := NewChannelHistory(book).Read(t.Context(), conversation, "", 200)
			if err != nil || len(got.Replies) != 2 || got.Conversation.Count != 2 {
				t.Fatalf("actual history: %+v %v", got, err)
			}
			delivery := "confirmed"
			if mode == "suppressed" {
				delivery = "suppressed"
			}
			if got.Replies[1].Delivery != delivery {
				t.Fatalf("delivery: %+v", got.Replies)
			}
		})
	}
}

func TestChannelHistoryLatestPageProjectAndLastAt(t *testing.T) {
	book := historyBook(t)
	for _, id := range []string{"a", "b", "c"} {
		historyInput(t, book, id, "chat", id)
	}
	historyRecord(t, book, "a/dispatch", "gateway-input-dispatch", recoveredOutput{Result: turn.Result{Text: "old", Injected: &turn.Injected{Project: "old-project", Agent: "old-agent"}}})
	historyRecord(t, book, "b/dispatch", "gateway-input-dispatch", recoveredOutput{Result: turn.Result{Text: "new", Injected: &turn.Injected{Project: "new-project", Agent: "new-agent"}}})
	historyProof(t, book, "b")
	// Variable fractional-second width must not reverse chronological order.
	for id, at := range map[string]string{"a": "2026-09-21T01:02:03Z", "b": "2026-09-21T01:02:03.001Z", "c": "2026-09-21T01:02:03.01Z", "b/reply": "2026-09-21T01:02:04Z"} {
		if _, err := book.DB().Exec(`UPDATE commands SET received_at=?,finished_at=? WHERE id=?`, at, at, id); err != nil {
			t.Fatal(err)
		}
	}
	h := NewChannelHistory(book)
	got, err := h.Read(t.Context(), "chat", "", 2)
	if err != nil || len(got.Replies) != 3 || got.NextCursor == "" || got.Replies[0].Input != "b" || got.Replies[2].Input != "c" {
		t.Fatalf("latest ascending page: %+v %v", got, err)
	}
	if got.Conversation.Project != "new-project" || got.Replies[1].ProjectID != "new-project" || got.Replies[1].Injected.Agent != "new-agent" || got.Conversation.LastAt.Format(time.RFC3339Nano) != "2026-09-21T01:02:04Z" {
		t.Fatalf("metadata: %+v", got)
	}
	older, err := h.Read(t.Context(), "chat", got.NextCursor, 2)
	if err != nil || len(older.Replies) != 2 || older.Replies[0].Input != "a" || older.NextCursor != "" || older.Conversation.Project != "new-project" {
		t.Fatalf("older page metadata: %+v %v", older, err)
	}
	list, err := h.List(t.Context(), "", 200)
	if err != nil || list.Conversations[0].Project != "new-project" {
		t.Fatalf("list metadata: %+v %v", list, err)
	}
}

func TestChannelHistoryCursorValidationAndLimit(t *testing.T) {
	book := historyBook(t)
	for i := 0; i < 205; i++ {
		historyInput(t, book, fmt.Sprintf("%03d", i), "chat", "input")
	}
	h := NewChannelHistory(book)
	got, err := h.Read(t.Context(), "chat", "", 999)
	if err != nil || len(got.Replies) != 200 || got.NextCursor == "" {
		t.Fatalf("limit: %d %v", len(got.Replies), err)
	}
	for _, raw := range []string{"!invalid", "e30", encodeChannelHistoryCursor(channelHistoryCursor{Version: 2, Kind: "read", Conversation: "chat", At: "2026-09-21T00:00:00", ID: "a"}), encodeChannelHistoryCursor(channelHistoryCursor{Version: 1, Kind: "read", Conversation: "chat", At: "invalid", ID: "a"})} {
		if _, err := h.Read(t.Context(), "chat", raw, 1); !errors.Is(err, consoleapi.ErrChannelHistoryCursor) {
			t.Fatalf("bad cursor accepted: %q %v", raw, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.Read(ctx, "chat", "", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}

func TestChannelHistoryErrorsDoNotExposeInternalDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		canceled   bool
	}{
		{name: "internal"}, {name: "user", user: "Please select a project."}, {name: "canceled", canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book := historyBook(t)
			historyInput(t, book, "input", "chat", "prompt")
			historyRecord(t, book, "input/dispatch", "gateway-input-dispatch", recoveredOutput{
				Result: turn.Result{Text: "partial result not disclosed by the final card"},
				Error:  "open /private/secret-config: token=do-not-disclose", UserError: tc.user, Canceled: tc.canceled,
			})
			got, err := NewChannelHistory(book).Read(t.Context(), "chat", "", 1)
			if err != nil || len(got.Replies) != 2 {
				t.Fatalf("reply: %+v %v", got, err)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "secret-config") || strings.Contains(string(raw), "do-not-disclose") || got.Replies[1].Error == "" {
				t.Fatalf("unsafe or missing error: %s", raw)
			}
			if tc.user != "" && got.Replies[1].Error != tc.user {
				t.Fatalf("public error lost: %+v", got.Replies[1])
			}
			if got.Replies[1].Text != got.Replies[1].Error {
				t.Fatalf("history must show the public final error instead of an undisclosed partial result: %+v", got.Replies[1])
			}
		})
	}
}

func TestChannelHistoryPendingRecoveryIsNotFinalOutput(t *testing.T) {
	for _, prefix := range []string{"gateway-input", "gateway-recovery"} {
		for _, delivered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delivered=%v", prefix, delivered), func(t *testing.T) {
				book := historyBook(t)
				if prefix == "gateway-input" {
					historyInput(t, book, "input", "chat", "prompt")
				} else {
					historyRecord(t, book, "input", recoveryInputKind, recoveryInput{
						Revival: Revival{ConversationID: "chat", MessageID: "input", Requester: "owner"},
						Prompt:  "prompt",
					})
				}
				historyRecord(t, book, "input/dispatch", prefix+"-dispatch", recoveredOutput{
					Recover: true, Result: turn.Result{Text: "obsolete partial", Injected: &turn.Injected{Project: "obsolete-project"}},
				})
				if delivered {
					historyRecord(t, book, "input/reply", prefix+"-reply", ledger.CommandProof{CommandID: "input", Receipt: "recovered-reply"})
				}
				h := NewChannelHistory(book)
				got, err := h.Read(t.Context(), "chat", "", 10)
				wantCount := 1
				if delivered {
					wantCount = 2
				}
				if err != nil || len(got.Replies) != wantCount || got.Conversation.Count != wantCount || got.Conversation.Project != "" {
					t.Fatalf("pending recovery became final history: %+v %v", got, err)
				}
				if delivered && (got.Replies[1].Text != "" || got.Replies[1].Delivery != "confirmed" || got.Replies[1].Injected != nil) {
					t.Fatalf("recovery proof must not confirm obsolete dispatch content: %+v", got.Replies[1])
				}
				list, err := h.List(t.Context(), "", 10)
				if err != nil || len(list.Conversations) != 1 || list.Conversations[0].Count != wantCount || list.Conversations[0].Project != "" {
					t.Fatalf("directory includes obsolete dispatch: %+v %v", list, err)
				}
			})
		}
	}
}

func TestChannelHistoryRunningDispatchAndRebuiltSnapshotIndexes(t *testing.T) {
	book := historyBook(t)
	historyInput(t, book, "input", "console:opaque", "accepted before execution")
	if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at) VALUES('input/dispatch','gateway-input-dispatch','owner','2026-09-21T01:02:03.000000001Z')`); err != nil {
		t.Fatal(err)
	}
	h := NewChannelHistory(book)
	got, err := h.Read(t.Context(), "console:opaque", "", 1)
	if err != nil || len(got.Replies) != 1 || got.Conversation.Count != 1 || got.Conversation.Running {
		t.Fatalf("in-flight receipt became output or live execution authority: %+v %v", got, err)
	}
	for _, name := range []string{"commands_channel_input", "commands_channel_input_id", "commands_channel_topic", "commands_channel_topic_id", "commands_channel_dispatch", "commands_channel_proof"} {
		if _, err := book.DB().Exec(`DROP INDEX ` + name); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := book.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	target := historyBook(t)
	if err := target.RestoreReplica(snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := NewChannelHistory(target).Read(t.Context(), "console:opaque", "", 1)
	if err != nil || len(restored.Replies) != 1 || restored.Replies[0].Input != got.Replies[0].Input {
		t.Fatalf("snapshot-derived indexes: %+v %v", restored, err)
	}
	var count int
	if err := target.DB().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name LIKE 'commands_channel_%'`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("indexes not reinstalled: %d %v", count, err)
	}
}
