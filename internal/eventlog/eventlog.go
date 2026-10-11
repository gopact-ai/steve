// Package eventlog is the atomic fact/outbox storage port. It depends on no
// subscriber or Work dispatcher; domains may publish without importing routing.
package eventlog

import (
	"encoding/json"
	"errors"
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ingress"
	"github.com/gopact-ai/steve/internal/ledger"
	"time"
)

const EventKind = "business-event"
const OutboxKind = "business-event-outbox"

type Event struct {
	ID               string          `json:"id"`
	Source           string          `json:"source"`
	Key              string          `json:"key"`
	Type             string          `json:"type"`
	Subject          string          `json:"subject,omitempty"`
	Payload          json.RawMessage `json:"payload"`
	Causation        string          `json:"causation,omitempty"`
	Depth            int             `json:"depth"`
	At               time.Time       `json:"at"`
	Attention        bool            `json:"attention"`
	RequestedVersion string          `json:"schema_version,omitempty"`
	Contract         *Contract       `json:"contract,omitempty"`
}

type Contract struct {
	Version     string          `json:"version"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Digest      string          `json:"digest"`
}

func AppendTx(tx *ledger.Tx, event Event) (Event, error) {
	if !identity.ValidName(event.Type) || event.Source == "" || event.Key == "" || len(event.Key) > 512 || !json.Valid(event.Payload) || len(event.Payload) > 256<<10 || event.Depth > 8 || event.Depth < 0 {
		return Event{}, errors.New("events: invalid event envelope or causal depth exceeds 8")
	}
	event.ID = durable.Key(event.Source, event.Key)
	// A duplicate must match the immutable event, including its type and causal
	// authority. At is reception time, so is deliberately excluded from identity.
	payload, err := json.Marshal(struct {
		Type, Subject, Causation string
		RequestedVersion         string `json:"RequestedVersion,omitempty"`
		Attention                bool
		Depth                    int
		Payload                  json.RawMessage
	}{event.Type, event.Subject, event.Causation, event.RequestedVersion, event.Attention, event.Depth, event.Payload})
	if err != nil {
		return Event{}, err
	}
	_, replay, err := ingress.AcceptTx(tx, ingress.Input{Kind: "event", Source: event.Source, Key: event.Key, Actor: event.Source, Payload: payload}, event.ID, event.At)
	if err != nil {
		return Event{}, err
	}
	if replay {
		old, found, err := durable.Get[Event](tx, EventKind, event.ID)
		if err != nil {
			return Event{}, err
		}
		if !found {
			return Event{}, errors.New("event receipt has no committed event")
		}
		return old, nil
	}
	if err := tx.PutBinding(EventKind, event.ID, event); err != nil {
		return Event{}, err
	}
	return event, tx.PutBinding(OutboxKind, event.ID, event.ID)
}
