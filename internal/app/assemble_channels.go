package app

import (
	"context"
	"log/slog"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	messagechannel "github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/turn"
)

func assembleChannels(boot runtimeAssembly, storage ledgerAssembly, work executionAssembly, projection readModelAssembly, page consoleAssembly, management administrationAssembly, delegates delegationAssembly) (channelsAssembly, error) {
	book := boot.Book()
	cfg := boot.Config()
	ctx := boot.Context()
	attempts := storage.Attempts()
	catalogText := work.CatalogText()
	coordinator := work.Coordinator()
	gw := work.Gateway()
	intents := work.Intents()
	view := projection.View()
	admin := page.Admin()
	cons := page.Console()
	channelSettings := management.ChannelSettings()
	gate := delegates.Gate()
	if gate != nil {
		gate.SetIntents(intent.ForAgents{S: intents, Attempts: attempts})
		// Platform questions are answered by the coordinator, live.
		gate.SetInformer(adminsvc.CoordinatorInformer{C: coordinator})
		gate.SetFleeter(adminsvc.FleetTools{Admin: admin, View: view})
		gate.SetMemorizer(coordinator)
	}
	var channel *feishu.Channel
	var err error
	if cfg.FeishuEnabled() {
		connect, cancelConnect := context.WithTimeout(ctx, 15*time.Second)
		channel, err = feishu.New(connect, feishu.Options{
			AppID:            cfg.Feishu.AppID,
			AppSecret:        cfg.Feishu.AppSecret,
			Domain:           cfg.Feishu.Domain,
			Access:           feishu.AccessFrom(cfg.Feishu),
			AllowUnmentioned: cfg.Feishu.AllowUnmentioned,
			OnCardAction:     gw.HandleCardAction,
		}, gw.HandleMessage)
		cancelConnect()
		if err != nil {
			slog.Error("steve: Feishu initialization failed; Console remains available")
			channelSettings.SetRuntimeError("Feishu initialization failed; check the application credentials and restart the Hub")
			channel = nil
		} else {
			gw.BindChannel(channel)
			channel.SetJournal(book.Journal())
		}
	}
	if gate != nil {
		gate.SetDefaultChannel(cfg.Gateway.DefaultChannel)
		if channel != nil {
			gate.BindChannel("feishu", feishu.Messenger{API: channel})
		}
		gate.BindChannel("console", console.MessageSender{Console: cons})
		cons.SetAnchorer(func(conversation, _ string, message string) {
			gate.Anchor(conversation, messagechannel.Address{Channel: "console", Conversation: conversation, Message: message})
		})
	}

	// /tasks resume re-enters through the same path a crash recovery does:
	// a notice at the anchor becomes the new anchor, and the continuation
	// arrives as an ordinary message.
	coordinator.SetOfflineReminder(time.Duration(cfg.Gateway.OfflineReminderAfter))
	coordinator.SetNotifier(func(n turn.TaskNotice) {
		err := routeTask(n.Transport, func() error { cons.Notice(n); return nil }, func() error {
			gw.Notify(gateway.Notice{TaskID: n.TaskID, MessageID: n.MessageID, Requester: n.Requester, Text: n.Text})
			return nil
		})
		if err != nil {
			slog.Error("task notice not routed", "task", n.TaskID, "error", err)
		}
	})
	coordinator.SetResumer(func(r turn.TaskResume) error {
		return routeTask(r.Transport, func() error {
			return cons.Resume(ctx, r.ConversationID, r.TaskID, r.Member, catalogText.T(i18n.TaskResumeNotice, r.TaskID), catalogText.T(i18n.TaskResumeManual, r.Goal), r.Admission, coordinator.ReviveSession)
		}, func() error {
			return gw.QueueTaskResume(ctx, book, r.Admission.ID, gateway.Revival{TaskID: r.TaskID, Goal: r.Goal, Member: r.Member, ConversationID: r.ConversationID, ChatID: r.ChatID, MessageID: r.MessageID, Requester: r.Requester, ChatType: r.ChatType, Manual: true}, r.Admission)
		})
	})
	gw.SetRecoveryLedger(book)
	gw.SetIngressLifetime(ctx, page.Reconciliations())
	coordinator.SetResumeDispatcher(func(r turn.TaskResume) {
		// Acceptance and owner authorization are already durable. Waking a
		// consumer is best-effort; startup/runtime recovery uses the same input.
		page.Reconciliations().Go(func() {
			err := routeTask(r.Transport, func() error { return cons.DispatchResume(r.ConversationID, r.Admission) }, func() error {
				return gw.DispatchResume(ctx, book, r.Admission, coordinator, coordinator.ReviveSession)
			})
			if err != nil {
				slog.Error("accepted task resume awaits recovery", "task", r.TaskID, "admission", r.Admission.ID, "error", err)
			}
		})
	})
	return &channelsValues{channel: channel}, nil
}

type channelsAssembly interface {
	Channel() *feishu.Channel
}

type channelsValues struct {
	channel *feishu.Channel
}

func (v *channelsValues) Channel() *feishu.Channel { return v.channel }
