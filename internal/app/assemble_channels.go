package app

import (
	"errors"
	"log/slog"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agentmcp"
	messagechannel "github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/turn"
)

func assembleChannels(boot runtimeAssembly, storage ledgerAssembly, work executionAssembly, projection readModelAssembly, page consoleAssembly, management administrationAssembly, delegates delegationAssembly) (channelsAssembly, error) {
	book := boot.Book()
	cfg := boot.Config()
	ctx := boot.Context()
	attempts := storage.Attempts()
	coordinator := work.Coordinator()
	gw := work.Gateway()
	intents := work.Intents()
	view := projection.View()
	admin := page.Admin()
	cons := page.Console()
	channelSettings := management.ChannelSettings()
	gate := delegates.Gate()
	taskChannels := delegates.TaskChannels()
	if taskChannels == nil {
		return nil, errors.New("channels require their registered task receivers")
	}
	if gate != nil {
		gate.SetIntents(intent.ForAgents{S: intents, Attempts: attempts})
		// Platform questions are answered by the coordinator, live.
		gate.SetInformer(adminsvc.CoordinatorInformer{C: coordinator})
		configurePlatformAuthority(gate, book, work.Accounts(), admin, view)
		gate.SetMemorizer(coordinator)
	}
	var channel *feishu.Channel
	if cfg.FeishuEnabled() {
		// runChannel starts the channel. Until its identity is verified
		// nothing answers or sends through it: Feishu input, recovery and
		// schedules wait instead of running work whose answer cannot be sent.
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
			OnReady:     func() { connectFeishu(gw, gate, channelSettings, channel) },
			OnReconnect: reportReconnect(channelSettings, cfg.Feishu.AppSecret),
		}, gw.HandleMessage)
		channel.SetJournal(book.Journal())
		channelSettings.BindAccessUpdater(func(f config.Feishu) {
			channel.SetAccess(feishu.AccessFrom(f), f.AllowUnmentioned)
		})
	}
	if gate != nil {
		gate.SetDefaultChannel(cfg.Gateway.DefaultChannel)
		gate.SetScheduler(work.Scheduler())
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
		err := taskChannels.Notice(n)
		if err != nil {
			slog.Error("task notice not routed", "task", n.TaskID, "error", err)
		}
	}
	routes.Resumer = func(r turn.TaskResume) error {
		return taskChannels.Resume(ctx, r)
	}
	for _, instance := range work.NativeChannels() {
		instance.SetRecoveryLedger(book)
		if err := instance.RestoreRoutes(ctx, book); err != nil {
			return nil, err
		}
		instance.SetIngressLifetime(ctx, page.Reconciliations(), coordinator)
		if gate != nil {
			instance.SetAgentGate(gate)
		}
	}
	routes.ResumeDispatcher = func(r turn.TaskResume) {
		// Acceptance and owner authorization are already durable. Waking a
		// consumer is best-effort; startup/runtime recovery uses the same input.
		page.Reconciliations().Go(func() {
			err := taskChannels.DispatchResume(ctx, r)
			if err != nil {
				slog.Error("accepted task resume awaits recovery", "task", r.TaskID, "admission", r.Admission.ID, "error", err)
			}
		})
	}

	return &channelsValues{channel: channel, routes: routes, startup: channelStartup{
		owner: cfg.Feishu.OwnerOpenID, home: cfg.Gateway.HomePath, timeout: time.Duration(cfg.Gateway.PromptTimeout),
	}}, nil
}

// connectFeishu makes a verified Feishu channel the one the gateway answers
// through and agents send through, and clears the startup retry shown in the
// console.
func connectFeishu(gw *gateway.Gateway, gate *agentmcp.Server, settings channelRuntime, ch gateway.Channel) {
	gw.BindChannel(ch)
	if gate != nil {
		gate.BindChannel("feishu", feishu.Messenger{API: ch})
	}
	settings.SetStartupRetry(nil)
}

// reportReconnect shows a Feishu long connection being established again
// in the console, without secret, and withdraws it once it is back.
func reportReconnect(settings channelRuntime, secret string) func(*feishu.Reconnect) {
	return func(r *feishu.Reconnect) {
		if r == nil {
			settings.SetReconnect(nil)
			return
		}
		shown := &consoleapi.ChannelReconnect{Since: r.Since, Attempts: r.Failures, LastAttemptAt: r.LastAttempt}
		if r.Err != nil {
			shown.LastError = adminsvc.RedactChannelError(r.Err, secret).Error()
		}
		settings.SetReconnect(shown)
	}
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
