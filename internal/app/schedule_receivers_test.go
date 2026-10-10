package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/schedule"
)

type scheduleReceiverFunc func(context.Context, schedule.Firing) (string, error)

func (f scheduleReceiverFunc) ReceiveSchedule(ctx context.Context, firing schedule.Firing) (string, error) {
	return f(ctx, firing)
}

func scheduledFiring(t *testing.T, channelName string) (*ledger.Ledger, *schedule.Store, schedule.Firing, time.Time) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	store, err := schedule.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Minute)
	_, err = store.Create(schedule.Job{Channel: channelName, ProjectID: "p", ConversationID: "conversation", ChatID: "chat", ChatType: "group", Requester: "owner", Member: "builder", Prompt: "work", AnchorMessage: "anchor", Spec: schedule.Spec{Kind: schedule.KindOnce, At: at}})
	if err != nil {
		t.Fatal(err)
	}
	due, err := store.Due(at)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due = %v, %v", due, err)
	}
	if err := store.BeginFiring(due[0].Key); err != nil {
		t.Fatal(err)
	}
	return book, store, due[0], at
}

func persistedFiring(t *testing.T, book *ledger.Ledger, key string) schedule.Firing {
	t.Helper()
	raw, _, err := book.Document("schedules").Load()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Firings map[string]schedule.Firing `json:"firings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	firing, ok := doc.Firings[key]
	if !ok {
		t.Fatal("firing missing from durable document")
	}
	return firing
}

func TestRegisteredThirdScheduleChannelPreservesFiringAndReceipt(t *testing.T) {
	book, store, firing, _ := scheduledFiring(t, "third")
	receivers := &schedule.ReceiverRegistry{}
	calls := 0
	if err := receivers.Register("third", scheduleReceiverFunc(func(_ context.Context, got schedule.Firing) (string, error) {
		calls++
		if !reflect.DeepEqual(got, firing) {
			t.Fatalf("frozen firing changed: %+v", got)
		}
		return "third:accepted", nil
	})); err != nil {
		t.Fatal(err)
	}
	dispatchFiring(t.Context(), store, receivers, nil, firing)
	saved := persistedFiring(t, book, firing.Key)
	if calls != 1 || saved.State != schedule.FiringAccepted || saved.Receipt != "third:accepted" || saved.AcceptedAt.IsZero() {
		t.Fatalf("handoff was not durably accepted: calls=%d %+v", calls, saved)
	}
	if _, exists := store.Get(firing.ID); exists {
		t.Fatal("accepted one-shot not consumed")
	}
}

func TestScheduleRegistryDeliveryFailuresPreserveOutcomeSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, receipt string
		err           error
		register      bool
		state         string
	}{
		{name: "unregistered", state: schedule.FiringPending},
		{name: "known rejection", err: errors.New("rejected"), register: true, state: schedule.FiringPending},
		{name: "unknown", err: errors.Join(channel.ErrOutcomeUnknown, errors.New("reply lost")), register: true, state: schedule.FiringUnknown},
		{name: "empty receipt", register: true, state: schedule.FiringUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book, store, firing, at := scheduledFiring(t, "third")
			receivers := &schedule.ReceiverRegistry{}
			if tc.register {
				if err := receivers.Register("third", scheduleReceiverFunc(func(context.Context, schedule.Firing) (string, error) { return tc.receipt, tc.err })); err != nil {
					t.Fatal(err)
				}
			}
			dispatchFiring(t.Context(), store, receivers, nil, firing)
			saved := persistedFiring(t, book, firing.Key)
			if saved.State != tc.state || saved.Error == "" || saved.Receipt != "" {
				t.Fatalf("failure changed semantics: %+v", saved)
			}
			again, err := schedule.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			due, err := again.Due(at)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.state == schedule.FiringUnknown {
				want = 0
			}
			if len(due) != want {
				t.Fatalf("recovery due=%v, want %d", due, want)
			}
			if !tc.register && !strings.Contains(saved.Error, `schedule channel "third" is not configured`) {
				t.Fatal(saved.Error)
			}
		})
	}
}

func TestThirdChannelRegistrationDoesNotGrantCrashReplay(t *testing.T) {
	book, _, firing, at := scheduledFiring(t, "third")
	receivers := &schedule.ReceiverRegistry{}
	calls := 0
	if err := receivers.Register("third", scheduleReceiverFunc(func(context.Context, schedule.Firing) (string, error) { calls++; return "external", nil })); err != nil {
		t.Fatal(err)
	}
	// Receiver accepts, then the process dies before AcceptFiring saves receipt.
	if _, err := receivers.ReceiveSchedule(t.Context(), firing); err != nil {
		t.Fatal(err)
	}
	restarted, err := schedule.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	due, err := restarted.Due(at)
	if err != nil || len(due) != 0 || calls != 1 {
		t.Fatalf("external delivery replayed: %v %v calls=%d", due, err, calls)
	}
	if saved := persistedFiring(t, book, firing.Key); saved.State != schedule.FiringUnknown {
		t.Fatalf("crash gap lost: %+v", saved)
	}
}

type capturingScheduleGateway struct{ got gateway.Fire }

func (c *capturingScheduleGateway) FireSchedule(_ context.Context, f gateway.Fire) (gateway.FireReceipt, error) {
	c.got = f
	return gateway.FireReceipt{MessageID: "announcement"}, nil
}

func TestGatewayScheduleAdapterPreservesAuthorityAndAnnouncementReceipt(t *testing.T) {
	book, store, firing, _ := scheduledFiring(t, "feishu")
	chat := &capturingScheduleGateway{}
	dispatchFiring(t.Context(), store, testScheduleReceivers(t, nil, chat), nil, firing)
	want := gateway.Fire{Channel: firing.Channel, ProjectID: firing.ProjectID, ScheduleID: firing.ID, ConversationID: firing.ConversationID, ChatID: firing.ChatID, ChatType: firing.ChatType, MessageID: firing.AnchorMessage, Requester: firing.Requester, Member: firing.Member, Prompt: firing.Prompt}
	if chat.got != want {
		t.Fatalf("gateway admission changed: %+v", chat.got)
	}
	if saved := persistedFiring(t, book, firing.Key); saved.Receipt != "announcement" || saved.State != schedule.FiringAccepted {
		t.Fatalf("gateway receipt changed: %+v", saved)
	}
}
