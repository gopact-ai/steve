package ledger

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPendingCommandRequiresItsOwnSuccessfulProof(t *testing.T) {
	for _, failure := range []string{"missing", "unfinished", "error", "kind", "actor", "input", "receipt", "input-unfinished"} {
		t.Run(failure, func(t *testing.T) {
			book := historyBook(t)
			ctx := t.Context()
			if err := book.RecordCommand(ctx, "input", "input-kind", "owner", json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
			if failure == "input-unfinished" {
				if _, err := book.DB().Exec(`UPDATE commands SET finished_at=NULL WHERE id='input'`); err != nil {
					t.Fatal(err)
				}
			}
			if failure != "missing" {
				kind, actor, command, receipt := "reply-kind", "owner", "input", "external-receipt"
				switch failure {
				case "kind":
					kind = "another-kind"
				case "actor":
					actor = "another-actor"
				case "input":
					command = "another-input"
				case "receipt":
					receipt = ""
				}
				raw, _ := json.Marshal(CommandProof{CommandID: command, Receipt: receipt})
				if err := book.RecordCommand(ctx, "input/reply", kind, actor, raw); err != nil {
					t.Fatal(err)
				}
				switch failure {
				case "unfinished":
					_, err := book.DB().Exec(`UPDATE commands SET finished_at=NULL WHERE id='input/reply'`)
					if err != nil {
						t.Fatal(err)
					}
				case "error":
					_, err := book.DB().Exec(`UPDATE commands SET error='unknown delivery' WHERE id='input/reply'`)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			err := book.AcknowledgeCommand(ctx, "input", "input-kind", "owner", "input/reply", "reply-kind")
			if err == nil {
				t.Fatal("unproven input removed from pending")
			}
			if failure == "missing" && !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("missing proof: %v", err)
			}
			pending, err := book.PendingCommands(ctx, "input-kind")
			if err != nil || len(pending) != 1 || pending[0].ID != "input" {
				t.Fatalf("lost pending input: %+v %v", pending, err)
			}
		})
	}
}

func TestPendingCommandAcknowledgementIsDurableAndImmutable(t *testing.T) {
	dir := t.TempDir()
	book, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, input := range []string{"input", "other"} {
		if err := book.RecordCommand(ctx, input, "input-kind", "owner", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(CommandProof{CommandID: input, Receipt: "receipt-" + input})
		if err := book.RecordCommand(ctx, input+"/reply", "reply-kind", "owner", raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.AcknowledgeCommand(ctx, "input", "input-kind", "owner", "other/reply", "reply-kind"); !errors.Is(err, ErrConflict) {
		t.Fatalf("another successful reply acknowledged input: %v", err)
	}
	for range 2 {
		if err := book.AcknowledgeCommand(ctx, "input", "input-kind", "owner", "input/reply", "reply-kind"); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	pending, err := book.PendingCommands(ctx, "input-kind")
	if err != nil || len(pending) != 1 || pending[0].ID != "other" {
		t.Fatalf("ack did not survive restart: %+v %v", pending, err)
	}
	all, err := book.Commands(ctx, "input-kind")
	if err != nil || len(all) != 2 {
		t.Fatalf("ack deleted history: %+v %v", all, err)
	}
	raw, _ := json.Marshal(CommandProof{CommandID: "input", Receipt: "second-receipt"})
	if err := book.RecordCommand(ctx, "input/second", "reply-kind", "owner", raw); err != nil {
		t.Fatal(err)
	}
	if err := book.AcknowledgeCommand(ctx, "input", "input-kind", "owner", "input/second", "reply-kind"); !errors.Is(err, ErrConflict) {
		t.Fatalf("ack proof was replaced: %v", err)
	}
}

func TestPendingCommandsSeekOnlyPendingWithTenThousandCompleted(t *testing.T) {
	book := historyBook(t)
	// Poisoned dates prove completed payloads are not read or decoded. The
	// partial-index plan fixes candidate work independently of history size.
	if _, err := book.DB().Exec(`WITH RECURSIVE n(seq) AS (VALUES(1) UNION ALL SELECT seq+1 FROM n WHERE seq<10000)
		INSERT INTO commands(id,kind,actor,received_at,finished_at,result,error,acknowledged_by)
		SELECT 'old-'||seq,'input-kind','owner','invalid-date','invalid-date','invalid-json','','proof-'||seq FROM n`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pending-1", "pending-2"} {
		if err := book.RecordCommand(t.Context(), id, "input-kind", "owner", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := book.db.Query(`EXPLAIN QUERY PLAN `+pendingCommandsQuery, "input-kind")
	if err != nil {
		t.Fatal(err)
	}
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
	rows.Close()
	explain := strings.Join(plan, "\n")
	t.Log(explain)
	if !strings.Contains(explain, "SEARCH commands USING INDEX commands_pending_kind") || strings.Contains(explain, "TEMP B-TREE") {
		t.Fatalf("pending query scans or sorts history:\n%s", explain)
	}
	for range 2 {
		pending, err := book.PendingCommands(t.Context(), "input-kind")
		if err != nil || len(pending) != 2 {
			t.Fatalf("read completed history: %+v %v", pending, err)
		}
	}
	if err := validateReplicaSchema(book.db); err != nil {
		t.Fatalf("owner index cannot replicate: %v", err)
	}
}
