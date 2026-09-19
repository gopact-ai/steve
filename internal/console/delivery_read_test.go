package console

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/turn"
)

type receiptHandler struct{}

func (receiptHandler) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{Attempt: "original-attempt", Text: "durable answer"}, nil
}

func TestConsoleDeliveryUsesServerAttemptAndSurvivesTranscriptPruning(t *testing.T) {
	for _, commandID := range []string{"client-command", ""} {
		t.Run("key="+commandID, func(t *testing.T) { testConsoleDelivery(t, commandID) })
	}
}

func testConsoleDelivery(t *testing.T, commandID string) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	service := New(receiptHandler{}, "owner", nil)
	if err := service.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	reply, err := service.SendCommand(t.Context(), "main", "question", commandID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(reply)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if wire["attempt_id"] != "original-attempt" {
		t.Fatalf("reply omitted its server-generated attempt identity: %s", raw)
	}
	service.mu.Lock()
	service.replies[reply.Conversation] = nil
	for i := 0; i < keep+10; i++ {
		service.exchanges[reply.Conversation] = append(service.exchanges[reply.Conversation], &queuedExchange{
			Exchange: Exchange{ID: fmt.Sprintf("non-native-%d", i), Conversation: reply.Conversation, State: consoleapi.ExchangeDone},
		})
	}
	service.trimExchangesLocked(reply.Conversation)
	if len(service.exchanges[reply.Conversation]) != keep+1 {
		t.Fatalf("native proof was lost or all ordinary unkeyed history retained: %d", len(service.exchanges[reply.Conversation]))
	}
	err = service.save()
	service.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reloaded := New(receiptHandler{}, "owner", nil)
	if err := reloaded.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	err = book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		id, ok, err := ConfirmedAttemptDeliveryTx(tx, "original-attempt", reply.Conversation, AnchorMark+reply.ExchangeID)
		if err != nil || !ok || id != reply.ID {
			t.Fatalf("pruned transcript lost durable delivery: %q %t %v", id, ok, err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*consoleExchangeRecord){
		func(row *consoleExchangeRecord) { row.Receipt.AttemptID = "other" },
		func(row *consoleExchangeRecord) { row.Receipt.ExchangeID = "other" },
		func(row *consoleExchangeRecord) { row.Receipt.Conversation = "other" },
		func(row *consoleExchangeRecord) { row.Receipt.ID = "other" },
		func(row *consoleExchangeRecord) { row.Receipt.Silent = true },
		func(row *consoleExchangeRecord) { row.State = consoleapi.ExchangeRunning },
		func(row *consoleExchangeRecord) { row.Receipt = nil },
	} {
		var original []byte
		if err := book.DB().QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, consoleExchangeKind, reply.ExchangeID).Scan(&original); err != nil {
			t.Fatal(err)
		}
		var row consoleExchangeRecord
		if err := json.Unmarshal(original, &row); err != nil {
			t.Fatal(err)
		}
		mutate(&row)
		if err := book.PutBinding(t.Context(), consoleExchangeKind, reply.ExchangeID, row); err != nil {
			t.Fatal(err)
		}
		_ = book.Read(t.Context(), func(tx *ledger.ReadTx) error {
			if id, found, err := ConfirmedAttemptDeliveryTx(tx, "original-attempt", reply.Conversation, AnchorMark+reply.ExchangeID); found || id != "" {
				t.Fatalf("unrelated/pending evidence became delivery: %q %t %v", id, found, err)
			}
			return nil
		})
		if err := book.PutBinding(t.Context(), consoleExchangeKind, reply.ExchangeID, json.RawMessage(original)); err != nil {
			t.Fatal(err)
		}
	}
}
