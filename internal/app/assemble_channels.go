package app

import (
	"log/slog"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	messagechannel "github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
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
	if cfg.FeishuEnabled() {
		// The channel is bound before it connects; runChannel starts it.
		channel = feishu.New(feishu.Options{
			AppID:            cfg.Feishu.AppID,
			AppSecret:        cfg.Feishu.AppSecret,
			Domain:           cfg.Feishu.Domain,
			Access:           feishu.AccessFrom(cfg.Feishu),
			AllowUnmentioned: cfg.Feishu.AllowUnmentioned,
			OnCardAction:     gw.HandleCardAction,
			OnStartRetry: func(r feishu.StartRetry) {
				channelSettings.SetStartupRetry(&consoleapi.ChannelStartupRetry{
					Attempts: r.Failures, NextAt: r.Next,
					LastError: adminsvc.RedactChannelError(r.Err, cfg.Feishu.AppSecret).Error(),
				})
			},
		}, gw.HandleMessage)
		gw.BindChannel(channel)
		channel.SetJournal(book.Journal())
		channelSettings.BindAccessUpdater(func(f config.Feishu) {
			channel.SetAccess(feishu.AccessFrom(f), f.AllowUnmentioned)
		})
	}
	if gate != nil {
		gate.SetDefaultChannel(cfg.Gateway.DefaultChannel)
		gate.SetScheduler(work.Scheduler())
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
	var routes taskRoutes
	routes.Notifier = func(n turn.TaskNotice) {
		err := routeTask(n.Transport, func() error { cons.Notice(n); return nil }, func() error {
			gw.Notify(gateway.Notice{TaskID: n.TaskID, MessageID: n.MessageID, Requester: n.Requester, Text: n.Text})
			return nil
		})
		if err != nil {
			slog.Error("task notice not routed", "task", n.TaskID, "error", err)
		}
	}
	routes.Resumer = func(r turn.TaskResume) error {
		return routeTask(r.Transport, func() error {
			return cons.Resume(ctx, r.ConversationID, r.TaskID, r.Member, catalogText.T(i18n.TaskResumeNotice, r.TaskID), catalogText.T(i18n.TaskResumeManual, r.Goal), r.Admission, coordinator.ReviveSession)
		}, func() error {
			return gw.QueueTaskResume(ctx, book, r.Admission.ID, gateway.Revival{TaskID: r.TaskID, Goal: r.Goal, Member: r.Member, ConversationID: r.ConversationID, ChatID: r.ChatID, MessageID: r.MessageID, Requester: r.Requester, ChatType: r.ChatType, Manual: true}, r.Admission)
		})
	}
	gw.SetRecoveryLedger(book)
	gw.SetIngressLifetime(ctx, page.Reconciliations(), coordinator)
	routes.ResumeDispatcher = func(r turn.TaskResume) {
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
	}
	return &channelsValues{channel: channel, routes: routes, startup: channelStartup{
		owner: cfg.Feishu.OwnerOpenID, home: cfg.Gateway.HomePath, timeout: time.Duration(cfg.Gateway.PromptTimeout),
	}}, nil
}

type channelsAssembly interface {
	Channel() *feishu.Channel
	Startup() channelStartup
	Routes() taskRoutes
}

// taskRoutes are the coordinator's Notifier, Resumer and ResumeDispatcher,
// routed to the channel a task came from.
type taskRoutes struct {
	Notifier         func(turn.TaskNotice)
	Resumer          func(turn.TaskResume) error
	ResumeDispatcher func(turn.TaskResume)
}

type channelStartup struct {
	owner, home string
	timeout     time.Duration
}

type channelsValues struct {
	channel *feishu.Channel
	routes  taskRoutes
	startup channelStartup
}

func (v *channelsValues) Channel() *feishu.Channel { return v.channel }
func (v *channelsValues) Startup() channelStartup  { return v.startup }
func (v *channelsValues) Routes() taskRoutes       { return v.routes }
