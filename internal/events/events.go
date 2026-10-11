// Package events is the durable business bus. The UI's SSE stream is a derived
// projection and is deliberately not used for delivery or consumer receipts.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/eventlog"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/provider"
)

const (
	EventKind        = "business-event"
	OutboxKind       = "business-event-outbox"
	SubscriptionKind = "event-subscription"
	DeliveryKind     = "event-delivery"
	InboxKind        = "event-inbox"
	InboxOpenKind    = "event-inbox-open"
)

type Event = eventlog.Event

type Publish struct {
	Key       string          `json:"key"`
	Type      string          `json:"type"`
	Subject   string          `json:"subject,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	Causation string          `json:"causation,omitempty"`
	Version   string          `json:"schema_version,omitempty"`
}

type Subscription struct {
	ID        string `json:"id"`
	Revision  uint64 `json:"revision"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Source    string `json:"source,omitempty"`
	EventType string `json:"event_type"`
	// Derived events opt in explicitly, preventing ordinary rules from feeding
	// their own completion events back into intelligent work.
	IncludeDerived bool   `json:"include_derived"`
	Mode           string `json:"mode"`              // inbox, work, provider
	Handler        string `json:"handler,omitempty"` // enrolled provider grant ID
	Conversation   string `json:"conversation,omitempty"`
	Project        string `json:"project,omitempty"`
	Instruction    string `json:"instruction,omitempty"`
	// ReplaySafe is an owner declaration of a consumer's durable deduplication
	// contract. It is not inferred from installing or registering a provider.
	ReplaySafe bool `json:"replay_safe"`
}

type Delivery struct {
	ID          string       `json:"id"`
	Revision    uint64       `json:"revision"`
	EventID     string       `json:"event_id"`
	Rule        Subscription `json:"rule"`
	State       string       `json:"state"` // pending, claimed, accepted, done, failed, unknown, cancelled
	Token       string       `json:"claim_token,omitempty"`
	Incarnation uint64       `json:"incarnation,omitempty"`
	Runtime     string       `json:"runtime,omitempty"`
	ClaimUntil  time.Time    `json:"claim_until,omitzero"`
	Receipt     string       `json:"receipt,omitempty"`
	Detail      string       `json:"detail,omitempty"`
	Resolution  *Resolution  `json:"resolution,omitempty"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

type Inbox struct {
	ID         string    `json:"id"`
	EventID    string    `json:"event_id"`
	DeliveryID string    `json:"delivery_id,omitempty"`
	Reason     string    `json:"reason"`
	Detail     string    `json:"detail,omitempty"`
	State      string    `json:"state"` // open, resolved
	At         time.Time `json:"at"`
}

type Service struct {
	Book         *ledger.Ledger
	Identity     *identity.Service
	ValidateWork func(context.Context, string, string, string) error
	Runtime      string
	Limits       durable.Limits
	WaitAfter    time.Duration
	scan         durable.Scanner
}

// AppendTx is the domain outbox port. Its caller supplies a stable fact key and
// calls it in the transaction committing that fact, never after commit.
// AppendTx commits a domain fact and its business outbox atomically.
var AppendTx = eventlog.AppendTx

func (s *Service) Publish(ctx context.Context, p identity.Principal, input Publish) (out Event, err error) {
	err = s.Book.Update(ctx, func(tx *ledger.Tx) error {
		grant, err := s.Identity.Check(tx, p)
		if err != nil {
			return err
		}
		if !grant.AllowsEvent(input.Type) {
			return identity.ErrDenied
		}
		event := Event{Source: grant.Principal, Key: input.Key, Type: input.Type, Subject: input.Subject, Payload: input.Payload, Causation: input.Causation, RequestedVersion: input.Version, At: time.Now().UTC(), Attention: true}
		old, exists, err := durable.Get[Event](tx, EventKind, durable.Key(grant.Principal, input.Key))
		if err != nil {
			return err
		}
		if exists {
			event.Depth = old.Depth
		}
		if input.Causation != "" && !exists {
			// A provider can only derive an event from an event delivered to it.
			delivery, found, err := durable.Get[Delivery](tx, DeliveryKind, input.Causation)
			if err != nil {
				return err
			}
			if !found || delivery.Rule.Handler != grant.ID || delivery.State == StatePending || delivery.State == StateCancelled {
				return identity.ErrDenied
			}
			parent, found, err := durable.Get[Event](tx, EventKind, delivery.EventID)
			if err != nil {
				return err
			}
			if !found {
				return identity.ErrDenied
			}
			event.Depth = parent.Depth + 1
		}
		if exists {
			// The original contract was frozen at first admission. Schema updates
			// and token rotations do not rewrite an identical input's receipt.
			event.Contract = old.Contract
		} else {
			contract, declared, err := provider.Freeze(tx, grant.ID, input.Type, input.Version)
			if err != nil {
				return err
			}
			if declared {
				event.Contract = &eventlog.Contract{Version: contract.Version, Description: contract.Description, Schema: contract.Schema, Digest: contract.Digest}
			}
			if err := s.checkCapacityTx(tx, grant.Principal, 1); err != nil {
				return err
			}
		}
		out, err = AppendTx(tx, event)
		return err
	})
	return
}

func (s *Service) SaveSubscription(ctx context.Context, rule Subscription) (Subscription, error) {
	if !identity.ValidName(rule.ID) || rule.Name == "" || len(rule.Name) > 128 || !identity.ValidName(rule.EventType) && !identity.ValidName(trimPattern(rule.EventType)) {
		return rule, errors.New("events: subscription ID, name and event type are required")
	}
	switch rule.Mode {
	case "inbox":
	case "work":
		if rule.Conversation == "" || rule.Project == "" || rule.Instruction == "" || len(rule.Instruction) > 16<<10 {
			return rule, errors.New("work subscription needs conversation, project and instruction")
		}
		if s.ValidateWork != nil && rule.Enabled {
			if err := s.ValidateWork(ctx, rule.Conversation, rule.Project, rule.Instruction); err != nil {
				return rule, err
			}
		}
	case "provider":
		if rule.Handler == "" {
			return rule, errors.New("provider subscription needs an enrolled handler")
		}
	default:
		return rule, errors.New("mode must be inbox, work or provider")
	}
	err := s.Book.Update(ctx, func(tx *ledger.Tx) error {
		old, found, err := durable.Get[Subscription](tx, SubscriptionKind, rule.ID)
		if err != nil {
			return err
		}
		if found && old.Revision == rule.Revision+1 {
			copy := rule
			copy.Revision = old.Revision
			if reflect.DeepEqual(copy, old) {
				rule = old
				return nil
			}
		}
		if found && old.Revision != rule.Revision || !found && rule.Revision != 0 {
			return ledger.ErrConflict
		}
		if rule.Mode == "provider" && rule.Enabled {
			g, found, err := durable.Get[identity.Grant](tx, identity.GrantKind, rule.Handler)
			if err != nil {
				return err
			}
			if !found || !g.Enabled || g.Kind != "provider" {
				return identity.ErrDenied
			}
		}
		rule.Revision++
		return tx.PutBinding(SubscriptionKind, rule.ID, rule)
	})
	return rule, err
}

func trimPattern(s string) string {
	if len(s) > 2 && s[len(s)-2:] == ".*" {
		return s[:len(s)-2]
	}
	return ""
}

// Route is an atomic fanout: a committed event has either its outbox row, or
// frozen deliveries/Inbox entries. No plugin callback runs in this transaction.
func (s *Service) Route(ctx context.Context, id string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		_, found, err := durable.Get[string](tx, OutboxKind, id)
		if err != nil || !found {
			return err
		}
		event, found, err := durable.Get[Event](tx, EventKind, id)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("outbox event missing")
		}
		rules, err := tx.Bindings(SubscriptionKind)
		if err != nil {
			return err
		}
		matched := false
		for _, raw := range rules {
			var rule Subscription
			if err := json.Unmarshal(raw, &rule); err != nil {
				return err
			}
			if !rule.Enabled || !identity.Matches(rule.EventType, event.Type) || rule.Source != "" && rule.Source != event.Source || event.Depth > 0 && !rule.IncludeDerived {
				continue
			}
			matched = true
			if event.Depth >= 8 && rule.Mode != "inbox" {
				if err := putInbox(tx, event.ID, "", "causal_limit", "Event chain reached its depth limit; review the subscription before starting more work"); err != nil {
					return err
				}
				continue
			}
			delivery := Delivery{ID: durable.Key(event.ID, rule.ID, fmt.Sprint(rule.Revision)), EventID: event.ID, Rule: rule, State: StatePending, UpdatedAt: time.Now().UTC()}
			// Reprocessing an unhandled event never resets an existing delivery.
			if _, exists, err := durable.Get[Delivery](tx, DeliveryKind, delivery.ID); err != nil {
				return err
			} else if exists {
				continue
			}
			if rule.Mode == "inbox" {
				delivery.State = StateDone
				if err := putInbox(tx, event.ID, delivery.ID, "subscription", rule.Name); err != nil {
					return err
				}
			}
			if rule.Mode == "provider" {
				grant, exists, err := durable.Get[identity.Grant](tx, identity.GrantKind, rule.Handler)
				if err != nil {
					return err
				}
				if !exists || !grant.Enabled || grant.Kind != "provider" {
					delivery.State = StateCancelled
					delivery.Detail = "Assigned consumer has been retired; select another subscription before reprocessing"
				}
			}
			if delivery.State == StatePending {
				if err := durable.CheckStateCapacityTx(tx, DeliveryKind, "$.rule.handler", rule.Handler, []string{StatePending, StateClaimed, StateAccepted, StateUnknown}, s.Limits, 1); err != nil {
					return err
				}
			}
			if err := saveDelivery(tx, delivery); err != nil {
				return err
			}
		}
		if !matched && event.Attention {
			if err := putInbox(tx, event.ID, "", "unhandled", "No subscription accepted this event"); err != nil {
				return err
			}
		}
		return durable.Delete(tx, OutboxKind, id)
	})
}

// Reprocess is an owner action after creating a subscription for an Inbox
// event. Frozen deliveries under existing rule revisions keep their receipts.
func (s *Service) Reprocess(ctx context.Context, id string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		event, err := restoreEventTx(tx, id)
		if err != nil {
			return err
		}
		if err := tx.PutBinding(OutboxKind, id, id); err != nil {
			return err
		}
		return s.checkCapacityTx(tx, event.Source, 0)
	})
}

func putInbox(tx *ledger.Tx, event, delivery, reason, detail string) error {
	id := durable.Key(event, delivery, reason)
	old, found, err := durable.Get[Inbox](tx, InboxKind, id)
	if err != nil {
		return err
	}
	if found {
		if old.State == StateOpen && old.Detail == detail {
			return nil
		}
		old.State = StateOpen
		old.Detail = detail
		if err := tx.PutBinding(InboxKind, id, old); err != nil {
			return err
		}
		return tx.PutBinding(InboxOpenKind, id, id)
	}
	if err := tx.PutBinding(InboxKind, id, Inbox{ID: id, EventID: event, DeliveryID: delivery, Reason: reason, Detail: detail, State: StateOpen, At: time.Now().UTC()}); err != nil {
		return err
	}
	return tx.PutBinding(InboxOpenKind, id, id)
}

func (s *Service) ResolveInbox(ctx context.Context, id string) error {
	return s.Book.Update(ctx, func(tx *ledger.Tx) error {
		item, found, err := durable.Get[Inbox](tx, InboxKind, id)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("Inbox entry not found")
		}
		item.State = StateResolved
		if err := tx.PutBinding(InboxKind, id, item); err != nil {
			return err
		}
		return durable.Delete(tx, InboxOpenKind, id)
	})
}
