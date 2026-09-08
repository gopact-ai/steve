package app

import (
	"context"
	"fmt"
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
		if n.ChatID == console.ChatID || console.IsConsole(n.MessageID) {
			cons.Notice(n)
			return
		}
		gw.Notify(gateway.Notice{
			TaskID: n.TaskID, MessageID: n.MessageID,
			Requester: n.Requester, Text: n.Text,
		})
	})
	coordinator.SetResumer(func(r turn.TaskResume) {
		if r.ChatID == console.ChatID || console.IsConsole(r.ConversationID) {
			if err := cons.Resume(ctx, r.ConversationID, r.TaskID, r.Member,
				catalogText.T(i18n.TaskResumeNotice, r.TaskID), catalogText.T(i18n.TaskResumeManual, r.Goal), coordinator.ReviveSession); err != nil {
				slog.Error(fmt.Sprintf("console: resume task #%s: %v", r.TaskID, err), "task", r.TaskID, "conversation", r.ConversationID)
			}
			return
		}
		go gw.ResumeTask(gateway.Revival{
			TaskID: r.TaskID, Goal: r.Goal, Member: r.Member,
			ConversationID: r.ConversationID, ChatID: r.ChatID,
			MessageID: r.MessageID, Requester: r.Requester,
			ChatType: r.ChatType, Manual: true,
		}, coordinator.ReviveSession)
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
