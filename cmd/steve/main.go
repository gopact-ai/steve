package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/debugapi"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/schedule"
	setupcmd "github.com/gopact-ai/steve/internal/setup"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"golang.org/x/term"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "setup":
			return setup(args[1:])
		case "doctor":
			return doctor(args[1:])
		case "run":
			args = args[1:]
		}
	}
	return serve(args)
}

func setup(args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to create")
	appID := flags.String("app-id", "", "Feishu/Lark app id")
	secretEnv := flags.String("app-secret-env", "FEISHU_APP_SECRET", "environment variable containing the app secret")
	domain := flags.String("domain", "", "feishu or lark")
	allowedSender := flags.String("allowed-sender", "", "optional group allowlist open_id; empty means open")
	blockedSender := flags.String("blocked-sender", "", "blocked sender open_id")
	groupPolicy := flags.String("group-policy", "", "open, allowlist, or disabled")
	allowUnmentioned := flags.Bool("allow-unmentioned", false, "listen to group messages without @ and let Steve decide whether to reply")
	ownerOpenID := flags.String("owner-open-id", "", "owner open_id (ou_...); the owner's DMs load the owner home and steve run opens the home chat")
	createApp := flags.Bool("create-app", false, "create a Feishu app via the official device-flow link")
	nonInteractive := flags.Bool("non-interactive", false, "do not prompt; require flags and env")
	if err := flags.Parse(args); err != nil {
		return err
	}
	interactive := isTerminal() && !*nonInteractive
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	_, err := setupcmd.Run(ctx, setupcmd.Flags{
		ConfigPath:       *configPath,
		AppID:            *appID,
		SecretEnv:        *secretEnv,
		Domain:           *domain,
		AllowedSender:    *allowedSender,
		BlockedSender:    *blockedSender,
		GroupPolicy:      *groupPolicy,
		AllowUnmentioned: *allowUnmentioned,
		CreateApp:        *createApp,
		OwnerOpenID:      *ownerOpenID,
	}, setupcmd.Options{Interactive: interactive})
	return err
}

func doctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	timeout := flags.Duration("timeout", 2*time.Minute, "total harness probe timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, catalog, manager, live, err := load(*configPath)
	if err != nil {
		return err
	}
	defer manager.Stop()
	store, err := state.Open(cfg.Gateway.StatePath)
	if err != nil {
		return err
	}
	if err := store.Check(); err != nil {
		return err
	}
	assembler, err := wireHome(cfg, live)
	if err != nil {
		return err
	}
	warnHome(cfg)
	if err := checkHome(cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	identity, err := feishu.Probe(ctx, cfg.Feishu.AppID, cfg.Feishu.AppSecret, cfg.Feishu.Domain)
	if err != nil {
		return err
	}
	log.Printf("steve: feishu bot %s %s", identity.Name, identity.OpenID)
	for _, selected := range catalog.List() {
		if _, err := assembler.AssembleMode(selected, home.ModeGuest); err != nil {
			return fmt.Errorf("agent %q guest home: %w", selected.ID, err)
		}
		if cfg.Feishu.OwnerOpenID != "" {
			if _, err := assembler.AssembleMode(selected, home.ModeOwner); err != nil {
				return fmt.Errorf("agent %q owner home: %w", selected.ID, err)
			}
		}
		capabilities, err := assembler.AssembleMode(selected, home.ModeGuest)
		if err != nil {
			return fmt.Errorf("agent %q capabilities: %w", selected.ID, err)
		}
		session, err := manager.OpenSession(ctx, selected.Harness, "", selected.Workspace, capabilities.MCPServers)
		if err != nil {
			return fmt.Errorf("agent %q session: %w", selected.ID, err)
		}
		if err := manager.CloseSession(ctx, selected.Harness, session.ID()); err != nil {
			return fmt.Errorf("agent %q close session: %w", selected.ID, err)
		}
	}
	if names := live.Map.EnabledNames(); len(names) > 0 {
		log.Printf("steve: skills %s", strings.Join(names, ","))
	} else {
		log.Printf("steve: skills none")
	}
	log.Printf("steve: doctor passed")
	return nil
}

// scheduleTick is how often standing work is checked. Twenty seconds is fine
// grain for a surface whose shortest interval is a minute, and cheap: due
// jobs are a map scan, and a tick with nothing due writes nothing.
const scheduleTick = 20 * time.Second

// runSchedules fires standing work until the gateway stops. Each run rotates
// the task the previous run opened, so a schedule that fires for weeks gets a
// fresh budget every time rather than spending one task's allowance a turn at
// a time.
func runSchedules(ctx context.Context, schedules *schedule.Store, gw *gateway.Gateway, coordinator *turn.Coordinator) {
	ticker := time.NewTicker(scheduleTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			due, err := schedules.Due(now)
			if err != nil {
				log.Printf("steve: claim due schedules: %v", err)
				continue
			}
			for _, job := range due {
				coordinator.RotateTask(job.ConversationID, job.Member, "schedule:"+job.ID)
				gw.FireSchedule(gateway.Fire{
					ScheduleID: job.ID, ConversationID: job.ConversationID,
					ChatID: job.ChatID, ChatType: job.ChatType,
					MessageID: job.AnchorMessage, Requester: job.Requester,
					Member: job.Member, Prompt: job.Prompt,
				})
			}
		}
	}
}

// staleTask is how long an interrupted task may sit before the gateway stops
// trying to continue it. Past a day the chat has moved on, and resuming would
// answer a question nobody is still asking.
const staleTask = 24 * time.Hour

func serve(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, catalog, manager, live, err := load(*configPath)
	if err != nil {
		return err
	}
	defer manager.Stop()
	// One gateway per state directory, enforced before anything connects:
	// two processes on one Feishu app split the event stream between them.
	unlock, err := runtime.AcquireLock(filepath.Dir(cfg.Gateway.StatePath))
	if err != nil {
		return err
	}
	defer unlock()
	store, err := state.Open(cfg.Gateway.StatePath)
	if err != nil {
		return err
	}
	assembler, err := wireHome(cfg, live)
	if err != nil {
		return err
	}
	warnHome(cfg)
	for _, selected := range catalog.List() {
		if _, err := assembler.AssembleMode(selected, home.ModeGuest); err != nil {
			return fmt.Errorf("agent %q guest home: %w", selected.ID, err)
		}
		if cfg.Feishu.OwnerOpenID != "" {
			if _, err := assembler.AssembleMode(selected, home.ModeOwner); err != nil {
				return fmt.Errorf("agent %q owner home: %w", selected.ID, err)
			}
		}
	}
	coordinator := turn.New(
		catalog, store, assembler, manager, time.Duration(cfg.Gateway.PromptTimeout),
	)
	catalogText := i18n.New(i18n.FromDomain(cfg.Feishu.Domain))
	coordinator.SetIdentity(cfg.Feishu.OwnerOpenID, home.Dir{Path: cfg.Gateway.HomePath})
	coordinator.SetSkills(live)
	tasks, err := task.Open(filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "tasks.json"))
	if err != nil {
		return fmt.Errorf("open tasks: %w", err)
	}
	tasks.SetBudget(cfg.Gateway.TaskMaxTurns, time.Duration(cfg.Gateway.TaskMaxElapsed))
	coordinator.SetTasks(tasks, nodeName())
	schedules, err := schedule.Open(filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "schedules.json"))
	if err != nil {
		return fmt.Errorf("open schedules: %w", err)
	}
	coordinator.SetSchedules(schedules)
	coordinator.SetCatalog(catalogText)
	if names := live.Map.EnabledNames(); len(names) > 0 {
		log.Printf("steve: isolated runtimes; skills=%s", strings.Join(names, ","))
	} else {
		log.Printf("steve: isolated runtimes; skills=none")
	}
	{
		log.Printf("steve: serving at most %d conversations at once", gateway.PoolSize())
	}
	gw := gateway.New(coordinator)
	gw.SetCatalog(catalogText)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The messaging server's URL is baked into session fingerprints, so the
	// port is remembered across restarts: losing it would ask every live
	// conversation for /new after each deploy.
	portPath := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "agentmcp.port")
	gate, err := agentmcp.New(readPort(portPath))
	if err != nil {
		// The send primitive is an enhancement; a box that cannot bind a
		// loopback port still serves ordinary turns.
		log.Printf("steve: agent messaging disabled: %v", err)
		gate = nil
	} else {
		if err := os.WriteFile(portPath, []byte(fmt.Sprintf("%d\n", gate.Port())), 0o600); err != nil {
			log.Printf("steve: remember agent messaging port: %v", err)
		}
		coordinator.SetAgentGate(gate)
		gw.SetAgentGate(gate)
		gate.SetJournal(func(conversationID, agentID, messageID string) {
			if err := tasks.AddInterim(conversationID, agentID, messageID); err != nil {
				log.Printf("steve: journal interim message: %v", err)
			}
		})
		go func() {
			if err := gate.Start(ctx); err != nil {
				log.Printf("steve: %v", err)
			}
		}()
		log.Printf("steve: agent messaging MCP server on %s", gate.URL())
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	channel, err := feishu.New(ctx, feishu.Options{
		AppID:            cfg.Feishu.AppID,
		AppSecret:        cfg.Feishu.AppSecret,
		Domain:           cfg.Feishu.Domain,
		Access:           feishu.AccessFrom(cfg.Feishu),
		AllowUnmentioned: cfg.Feishu.AllowUnmentioned,
		OnCardAction:     gw.HandleCardAction,
	}, gw.HandleMessage)
	if err != nil {
		return err
	}
	gw.BindChannel(channel)
	if gate != nil {
		gate.BindChannel(channel)
	}

	// /tasks resume re-enters through the same path a crash recovery does:
	// a notice at the anchor becomes the new anchor, and the continuation
	// arrives as an ordinary message.
	coordinator.SetOfflineReminder(time.Duration(cfg.Gateway.OfflineReminderAfter))
	coordinator.SetNotifier(func(n turn.TaskNotice) {
		gw.Notify(gateway.Notice{
			TaskID: n.TaskID, MessageID: n.MessageID,
			Requester: n.Requester, Text: n.Text,
		})
	})
	coordinator.SetResumer(func(r turn.TaskResume) {
		go gw.ResumeTask(gateway.Revival{
			TaskID: r.TaskID, Goal: r.Goal, Member: r.Member,
			ConversationID: r.ConversationID, ChatID: r.ChatID,
			MessageID: r.MessageID, Requester: r.Requester,
			ChatType: r.ChatType, Manual: true,
		}, coordinator.ReviveSession)
	})

	// Pick back up what a dead gateway left mid-turn: close the orphaned
	// attempt, revive the session, and continue through a real message so
	// the resumed turn renders a card like any other turn.
	var revivals []gateway.Revival
	var dropped []gateway.Notice
	for _, interrupted := range tasks.Interrupted() {
		if _, err := tasks.Finish(interrupted.ID, task.OutcomeInterrupted, task.Tokens{}, 0); err != nil {
			log.Printf("steve: close interrupted attempt #%s: %v", interrupted.ID, err)
			continue
		}
		// A paused task's attempt still had to be closed, but resuming it
		// would overrule the user who set it down.
		if interrupted.State == task.StatePaused {
			log.Printf("steve: task #%s is paused; leaving it set aside", interrupted.ID)
			continue
		}
		if interrupted.AnchorMessage == "" {
			// Nothing to reply to, so nothing can be said: the task is
			// only recoverable through the listing.
			log.Printf("steve: task #%s interrupted with no anchor; not resumable", interrupted.ID)
			continue
		}
		// A task that stops has to say so. Silence here is the one failure
		// the delivery promise cannot survive: the user asked for an hour of
		// work and would otherwise never learn it ended.
		if time.Since(interrupted.UpdatedAt) > staleTask {
			log.Printf("steve: task #%s interrupted long ago; leaving it stopped", interrupted.ID)
			dropped = append(dropped, gateway.Notice{
				TaskID: interrupted.ID, MessageID: interrupted.AnchorMessage,
				Requester: interrupted.Requester,
				Text: catalogText.T(i18n.TaskDropped, interrupted.ID,
					time.Since(interrupted.UpdatedAt).Round(time.Hour)),
			})
			continue
		}
		revivals = append(revivals, gateway.Revival{
			TaskID: interrupted.ID, Goal: interrupted.Goal, Member: interrupted.Member,
			ConversationID: interrupted.Channel, ChatID: interrupted.ChatID,
			MessageID: interrupted.AnchorMessage, Requester: interrupted.Requester,
			ChatType: interrupted.ChatType,
			OpenCard: interrupted.OpenCard, Interim: interrupted.Interim,
		})
	}
	if len(revivals) > 0 {
		go gw.Revive(revivals, coordinator.ReviveSession)
	}
	for _, notice := range dropped {
		go gw.Notify(notice)
	}

	go runSchedules(ctx, schedules, gw, coordinator)

	if addr := cfg.Gateway.DebugAddr; addr != "" {
		go func() {
			if err := debugapi.Serve(ctx, addr, gw, debugapi.Defaults{
				ChatID:       cfg.Gateway.DebugChatID,
				SenderOpenID: cfg.Feishu.OwnerOpenID,
				Sender:       channel,
			}); err != nil {
				log.Printf("steve: %v", err)
			}
		}()
	}

	go func() {
		onboardCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Gateway.PromptTimeout))
		defer cancel()
		if err := onboard.Start(onboardCtx, onboard.Request{
			Owner:   cfg.Feishu.OwnerOpenID,
			Home:    cfg.Gateway.HomePath,
			Store:   store,
			Catalog: catalogText,
			Handle: func(ctx context.Context, req onboard.TurnRequest) (onboard.TurnResult, error) {
				result, err := coordinator.Handle(ctx, turn.Request{
					ConversationID: req.ConversationID,
					Input:          req.Input,
					SenderOpenID:   req.SenderOpenID,
					ChatType:       protocol.ParseChatType(req.ChatType),
				})
				if err != nil {
					return onboard.TurnResult{}, err
				}
				return onboard.TurnResult{Text: result.Text}, nil
			},
			Send: func(ctx context.Context, receiveID, text string) (string, error) {
				sent, err := channel.Send(ctx, receiveID, text)
				if err != nil {
					return "", err
				}
				return sent.ChatID, nil
			},
		}); err != nil {
			log.Printf("steve: onboard: %v", err)
		}
	}()

	log.Printf("steve: starting Feishu long connection")
	if err := channel.Start(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return fmt.Errorf("start Feishu channel: %w", err)
	}
	return nil
}

func load(path string) (*config.Config, *agent.Catalog, *harness.Manager, *skills.Live, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := cfg.Feishu.Validate(); err != nil {
		return nil, nil, nil, nil, err
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stateDir := filepath.Dir(cfg.Gateway.StatePath)
	if err := runtime.Prepare(stateDir); err != nil {
		return nil, nil, nil, nil, err
	}
	skillMap, err := skills.Setup(stateDir)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	live := &skills.Live{Map: skillMap, Dests: runtime.SkillDests(stateDir)}
	if err := live.Apply(); err != nil {
		return nil, nil, nil, nil, err
	}
	for id, item := range cfg.Harnesses {
		item.Env = runtime.ApplyEnv(item.Env, id, stateDir)
		cfg.Harnesses[id] = item
	}
	manager, err := cfg.HarnessManager()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	live.After = manager.Restart
	return cfg, catalog, manager, live, nil
}

func wireHome(cfg *config.Config, live *skills.Live) (*capability.Assembler, error) {
	locale := home.LocaleZH
	if i18n.FromDomain(cfg.Feishu.Domain) == i18n.LocaleEN {
		locale = home.LocaleEN
	}
	if err := home.BootstrapLocale(cfg.Gateway.HomePath, cfg.Feishu.OwnerOpenID, locale); err != nil {
		return nil, err
	}
	assembler := cfg.CapabilityAssembler().SetHome(home.Dir{Path: cfg.Gateway.HomePath, Locale: locale})
	if live != nil && live.Map != nil {
		assembler.SetSkills(live.Map)
	}
	return assembler, nil
}

func warnHome(cfg *config.Config) {
	if cfg.Feishu.OwnerOpenID == "" {
		log.Printf("steve: feishu.owner_open_id is unset; DMs use guest home")
	}
}

func checkHome(cfg *config.Config) error {
	info, err := os.Stat(cfg.Gateway.HomePath)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		log.Printf("steve: home directory mode is %o, want 700", info.Mode().Perm())
	}
	for _, name := range []string{home.FileSoul, home.FileUser, home.FileMemory} {
		path := filepath.Join(cfg.Gateway.HomePath, name)
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if st.Mode().Perm() != 0o600 {
			log.Printf("steve: %s mode is %o, want 600", name, st.Mode().Perm())
		}
	}
	snap, err := home.Load(cfg.Gateway.HomePath, home.ModeOwner)
	if err != nil {
		return err
	}
	for _, warning := range snap.Warnings {
		log.Printf("steve: %s", warning)
	}
	user, err := os.ReadFile(filepath.Join(cfg.Gateway.HomePath, home.FileUser))
	if err != nil {
		return err
	}
	if strings.Contains(string(user), home.TemplateMarker) {
		log.Printf("steve: edit USER.md")
	}
	return nil
}

func isTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// readPort reads a previously remembered loopback port; 0 means none.
func readPort(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// nodeName labels which machine ran a turn. It is cosmetic today and load
// bearing once tasks can be placed on more than one node.
func nodeName() string {
	if name := strings.TrimSpace(os.Getenv("STEVE_NODE")); name != "" {
		return name
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "local"
	}
	return host
}
