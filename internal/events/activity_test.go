package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestActivityUsesReceiptTimeAndRetainsOldColdAttentionWithoutSecretsOrWrites(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	service := &Service{Book: book}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 80 {
		err := book.Update(t.Context(), func(tx *ledger.Tx) error {
			event, err := AppendTx(tx, Event{Source: "provider:activity", Key: fmt.Sprint(i), Type: "mail.received", Subject: fmt.Sprintf("Original message %d", i), At: base.Add(time.Duration(i) * time.Second), Payload: json.RawMessage(`{"secret":"` + strings.Repeat("private payload ", 60) + `"}`)})
			if err == nil {
				ids = append(ids, event.ID)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err = book.Update(t.Context(), func(tx *ledger.Tx) error {
		for _, id := range []string{ids[0], ids[79]} {
			if _, err := durable.ArchiveTx(tx, EventKind, id); err != nil {
				return err
			}
		}
		for i, state := range []string{StateFailed, StateUnknown} {
			delivery := Delivery{ID: "old-" + state, EventID: ids[i], State: state, Token: "private claim credential", Rule: Subscription{Name: "Frozen rule", Mode: "provider"}, UpdatedAt: base, Detail: "Original consumer evidence", Receipt: strings.Repeat("private receipt ", 60)}
			if err := tx.PutBinding(DeliveryKind, delivery.ID, delivery); err != nil {
				return err
			}
			if err := putInbox(tx, delivery.EventID, delivery.ID, state, delivery.Detail); err != nil {
				return err
			}
			if state == StateFailed {
				if _, err := durable.ArchiveTx(tx, DeliveryKind, delivery.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := service.Flow(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	activity, err := service.Activity(t.Context())
	if err != nil || len(activity.Recent) != 32 || len(activity.Attention) != 2 {
		t.Fatalf("bounded activity: %+v, %v", activity, err)
	}
	for i, item := range activity.Recent {
		if item.ID != ids[79-i] || item.Subject != fmt.Sprintf("Original message %d", 79-i) || item.Routed {
			t.Fatalf("recent order or original envelope changed at %d: %+v", i, item)
		}
	}
	for _, item := range activity.Attention {
		if item.Rule != "Frozen rule" || item.Detail != "Original consumer evidence" || item.EventID != ids[0] && item.EventID != ids[1] {
			t.Fatalf("old attention disappeared outside recent window: %+v", item)
		}
	}
	raw, err := json.Marshal(activity)
	if err != nil || strings.Contains(string(raw), "private") || strings.Contains(string(raw), "payload") || strings.Contains(string(raw), "claim_token") {
		t.Fatalf("digest leaked payload or credentials: %s, %v", raw, err)
	}
	after, err := service.Flow(t.Context())
	if err != nil || before.Queues["routing"].Pending != after.Queues["routing"].Pending {
		t.Fatalf("observation consumed pending events: %+v, %v", after, err)
	}
	for i := range 5 {
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
			delivery := Delivery{ID: fmt.Sprintf("extra-%d", i), EventID: ids[0], State: StateUnknown, Rule: Subscription{Name: "Frozen rule", Mode: "provider"}, UpdatedAt: base}
			if err := tx.PutBinding(DeliveryKind, delivery.ID, delivery); err != nil {
				return err
			}
			return putInbox(tx, delivery.EventID, delivery.ID, StateUnknown, "")
		}); err != nil {
			t.Fatal(err)
		}
	}
	activity, err = service.Activity(t.Context())
	if err != nil || len(activity.Attention) != 4 || !activity.MoreAttention {
		t.Fatalf("attention exceeded dashboard limit or concealed remainder: %+v, %v", activity, err)
	}
	// Resolved old attention must leave the digest, even though its delivery is
	// retained in the complete ledger for original-receipt inspection.
	page, err := service.InboxEntries(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if err := service.ResolveInbox(t.Context(), item.ID); err != nil {
			t.Fatal(err)
		}
	}
	activity, err = service.Activity(t.Context())
	if err != nil || len(activity.Attention) != 0 {
		t.Fatalf("resolved attention remained visible: %+v, %v", activity, err)
	}
}
