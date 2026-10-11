package app

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

func testTaskChannels(t *testing.T, page *console.Service, chat *gateway.Gateway, book *ledger.Ledger, coordinator *turn.Coordinator) *taskChannelRegistry {
	t.Helper()
	receivers, err := assembleTaskChannels(page, chat, book, coordinator, i18n.New(i18n.LocaleEN))
	if err != nil {
		t.Fatal(err)
	}
	return receivers
}

type recordingTaskReceiver struct {
	notices    int
	resume     turn.TaskResume
	firing     schedule.Firing
	delivery   delegate.Delivery
	receiptKey string
}

func (r *recordingTaskReceiver) Notice(turn.TaskNotice) error { r.notices++; return nil }
func (r *recordingTaskReceiver) Resume(_ context.Context, input turn.TaskResume) error {
	r.resume = input
	return nil
}
func (r *recordingTaskReceiver) DispatchResume(_ context.Context, input turn.TaskResume) error {
	r.resume = input
	return nil
}
func (r *recordingTaskReceiver) ReceiveSchedule(_ context.Context, input schedule.Firing) (string, error) {
	r.firing = input
	return "accepted", nil
}
func (r *recordingTaskReceiver) Deliver(_ context.Context, input delegate.Delivery, confirm func(error)) error {
	r.delivery = input
	confirm(channel.ErrOutcomeUnknown)
	return channel.ErrDeliveryQueued
}
func (*recordingTaskReceiver) ReplaySafe() bool { return false }
func (r *recordingTaskReceiver) ContinuationReceipt(_ task.Task, key string) (bool, error) {
	r.receiptKey = key
	return false, channel.ErrOutcomeUnknown
}

func TestTaskTransportNeverInfersDestination(t *testing.T) {
	for _, transport := range []string{"console", "feishu", "", "unknown", "console:message"} {
		t.Run(transport, func(t *testing.T) {
			page, chat := &recordingTaskReceiver{}, &recordingTaskReceiver{}
			receivers, err := newTaskChannels(map[string]taskReceiver{"console": page, "feishu": chat})
			if err != nil {
				t.Fatal(err)
			}
			err = receivers.Notice(turn.TaskNotice{Transport: transport, Conversation: "console:message", MessageID: "console:anchor"})
			switch transport {
			case "console":
				if err != nil || page.notices != 1 || chat.notices != 0 {
					t.Fatalf("page=%d chat=%d error=%v", page.notices, chat.notices, err)
				}
			case "feishu":
				if err != nil || page.notices != 0 || chat.notices != 1 {
					t.Fatalf("page=%d chat=%d error=%v", page.notices, chat.notices, err)
				}
			default:
				if err == nil || page.notices != 0 || chat.notices != 0 {
					t.Fatalf("unknown transport dispatched: page=%d chat=%d error=%v", page.notices, chat.notices, err)
				}
			}
		})
	}
}

func TestTaskChannelsPreserveThirdProviderAdmissionAndUncertainDelivery(t *testing.T) {
	mail, other := &recordingTaskReceiver{}, &recordingTaskReceiver{}
	registered := map[string]taskReceiver{"mail": mail, "console": other}
	receivers, err := newTaskChannels(registered)
	if err != nil {
		t.Fatal(err)
	}
	registered["mail"] = other // construction freezes the trusted adapter binding
	input := turn.TaskResume{Transport: "mail", TaskID: "task", Requester: "native-account", ConversationID: "console:opaque-thread", Admission: task.ResumeAdmission{ID: "original-input", TaskID: "task", Epoch: 7}}
	if err := receivers.Resume(t.Context(), input); err != nil || mail.resume != input {
		t.Fatalf("native resume identity changed: %+v %v", mail.resume, err)
	}
	if err := receivers.DispatchResume(t.Context(), input); err != nil || mail.resume != input {
		t.Fatalf("accepted input lost its original admission: %+v %v", mail.resume, err)
	}
	firing := schedule.Firing{Key: "firing", Channel: "mail", Requester: "native-account", ConversationID: "console:opaque-thread"}
	if receipt, err := receivers.ReceiveSchedule(t.Context(), firing); err != nil || receipt != "accepted" || mail.firing.Key != firing.Key || mail.firing.Requester != firing.Requester || mail.firing.ConversationID != firing.ConversationID {
		t.Fatalf("schedule lost the frozen handoff: %+v receipt=%s error=%v", mail.firing, receipt, err)
	}
	parent := task.Task{Transport: "mail", ID: "task", Channel: input.ConversationID}
	if receivers.ReplaySafe(parent) {
		t.Fatal("registration granted replay safety")
	}
	if confirmed, err := receivers.ContinuationReceipt(parent, "original-key"); confirmed || !errors.Is(err, channel.ErrOutcomeUnknown) || mail.receiptKey != "original-key" {
		t.Fatalf("uncertain receipt became confirmation: confirmed=%t error=%v", confirmed, err)
	}
	delivery := delegate.Delivery{Transport: "mail", ParentTask: "task", Conversation: input.ConversationID, Requester: "native-account", Key: "original-key"}
	var confirmed error
	err = receivers.Deliver(t.Context(), delivery, func(err error) { confirmed = err })
	if !errors.Is(err, channel.ErrDeliveryQueued) || !errors.Is(confirmed, channel.ErrOutcomeUnknown) || mail.delivery.Key != delivery.Key || mail.delivery.Requester != delivery.Requester || mail.delivery.Conversation != delivery.Conversation {
		t.Fatalf("handoff lost its key or uncertain confirmation: %+v %v %v", mail.delivery, err, confirmed)
	}
	if other.notices != 0 || other.resume.TaskID != "" || other.delivery.Key != "" || other.firing.Key != "" {
		t.Fatal("Console-looking IDs redirected provider work")
	}
}
