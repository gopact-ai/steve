// Package ingress owns immutable accepted input identities and adapter receipts.
// Adapters retain their own execution/recovery queues; an ingress receipt never
// proves an ACP attempt or an external reply completed.
package ingress

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

const RecordKind = "service-ingress"

var ErrConflict = errors.New("ingress: input identity was reused with different content or authority")

type Input struct {
	Kind         string          `json:"kind"`
	Source       string          `json:"source"`
	Key          string          `json:"key"`
	Actor        string          `json:"actor"`
	Conversation string          `json:"conversation,omitempty"`
	Payload      json.RawMessage `json:"payload"`
}

type Receipt struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Source         string    `json:"source"`
	Key            string    `json:"key"`
	Actor          string    `json:"actor"`
	Conversation   string    `json:"conversation,omitempty"`
	Digest         string    `json:"digest"`
	AcceptedAt     time.Time `json:"accepted_at"`
	AdapterReceipt string    `json:"adapter_receipt,omitempty"`
}

// AcceptTx participates in the adapter's own acceptance transaction. Payload is
// hashed rather than copied into another transcript or secret-bearing log.
func AcceptTx(tx *ledger.Tx, input Input, adapterReceipt string, at time.Time) (Receipt, bool, error) {
	if input.Kind == "" || input.Source == "" || input.Key == "" || input.Actor == "" || !json.Valid(input.Payload) {
		return Receipt{}, false, errors.New("ingress: kind, source, key, actor and JSON payload are required")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(input.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Receipt{}, false, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return Receipt{}, false, err
	}
	id := durable.Key(input.Kind, input.Source, input.Key)
	digest := durable.Key(input.Actor, input.Conversation, string(canonical))
	old, found, err := durable.Get[Receipt](tx, RecordKind, id)
	if err != nil {
		return Receipt{}, false, err
	}
	if found {
		if old.Digest != digest || old.AdapterReceipt != adapterReceipt {
			return Receipt{}, true, ErrConflict
		}
		return old, true, nil
	}
	record := Receipt{ID: id, Kind: input.Kind, Source: input.Source, Key: input.Key, Actor: input.Actor, Conversation: input.Conversation, Digest: digest, AcceptedAt: at, AdapterReceipt: adapterReceipt}
	return record, false, tx.PutBinding(RecordKind, id, record)
}
