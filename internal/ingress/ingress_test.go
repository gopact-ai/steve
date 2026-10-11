package ingress

import (
	"encoding/json"
	"errors"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
	"testing"
	"time"
)

func TestAcceptancePreservesLargeNumbersAndRollsBackWithAdapterAdmission(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	input := Input{Kind: "event", Source: "provider", Key: "message", Actor: "principal", Payload: json.RawMessage(`{"id":9007199254740993,"body":"Hi"}`)}
	fault := errors.New("adapter refused")
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		if _, _, err := AcceptTx(tx, input, "original", time.Now()); err != nil {
			return err
		}
		return fault
	}); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if _, found, err := durable.Read[Receipt](t.Context(), book, RecordKind, durable.Key("event", "provider", "message")); err != nil || found {
		t.Fatal("ingress escaped rolled back adapter admission")
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, replay, err := AcceptTx(tx, input, "original", time.Now())
		if replay {
			t.Fatal("rollback retained a receipt")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	input.Payload = json.RawMessage(`{"body":"Hi", "id":9007199254740993}`)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, replay, err := AcceptTx(tx, input, "original", time.Now())
		if !replay {
			t.Fatal("JSON key order changed identity")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	input.Payload = json.RawMessage(`{"body":"Hi", "id":9007199254740992}`)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, _, err := AcceptTx(tx, input, "original", time.Now()); return err }); !errors.Is(err, ErrConflict) {
		t.Fatalf("integer precision aliased two inputs: %v", err)
	}
}
