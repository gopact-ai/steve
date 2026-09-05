package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gopact-ai/steve/internal/mcpprobe"
	"github.com/gopact-ai/steve/internal/mcpregistry"
	"github.com/gopact-ai/steve/internal/mcpscan"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/models"
	steveview "github.com/gopact-ai/steve/internal/view"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gopact-ai/acp"
	gopactsqlite "github.com/gopact-ai/gopact-ext/stores/sqlite"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/debugapi"
	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/schedule"
	setupcmd "github.com/gopact-ai/steve/internal/setup"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/tui"
	"github.com/gopact-ai/steve/internal/turn"
	"golang.org/x/term"
)

// homeProjectID names Steve's home directory as a project.
const homeProjectID = config.ReservedHomeProject

// idleTaskAge is how long a chat thread may go unspoken to before its
// task is closed. A person can always start a new one by talking.
const idleTaskAge = 24 * time.Hour

// sweepIdleTasks closes chat tasks that have gone quiet, at start and
// then hourly, and puts each closing in the history.
func sweepIdleTasks(ctx context.Context, tasks *task.Store, attempts *attempt.Service, view *readmodel.Model) {
	live := func(id string) bool {
		_, ok := attempts.LiveAttemptOf(ctx, id)
		return ok
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		for _, t := range tasks.CloseIdle(idleTaskAge, live) {
			log.Printf("steve: task #%s closed after %s without a word", t.ID, idleTaskAge)
			if view != nil {
				view.Observe("task.idle", t.ID, fmt.Sprintf("task #%s (%s) closed: quiet for more than %s", t.ID, t.Member, idleTaskAge))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepAttempts keeps expiring attempts whose drivers stopped renewing.
func sweepAttempts(ctx context.Context, attempts *attempt.Service) {
	ticker := time.NewTicker(attempts.TTL)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expired, err := attempts.Sweep(ctx)
			if err != nil {
				log.Printf("steve: sweep attempts: %v", err)
			}
			for _, r := range expired {
				log.Printf("steve: expired attempt %s", attempt.Describe(r))
			}
		}
	}
}

// probeWorkspace picks a project directory on node for a doctor probe: the
// default project's if it is homed there, else any project's.
func probeWorkspace(projects []project.Project, node string) (string, bool) {
	for _, p := range projects {
		if p.ID == homeProjectID {
			continue
		}
		if ws, err := p.Place(node); err == nil {
			return ws.Path, true
		}
	}
	for _, p := range projects {
		if ws, err := p.Place(node); err == nil {
			return ws.Path, true
		}
	}
	return "", false
}

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
		case "top":
			return top(os.Args[2:])
		case "dash":
			return dash(os.Args[2:])
		case "ledger":
			return ledgerCmd(args[1:])
		case "say":
			return say(args[1:])
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
	book, err := openLedger(cfg)
	if err != nil {
		return err
	}
	defer book.Close()
	store, err := state.OpenLedger(book, cfg.Gateway.StatePath)
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

	// Nodes are probed before agents: availability is a real dial, not a
	// line in the config, and an agent placed on an unreachable node should
	// fail with that fact rather than with a mystery session error.
	nodewire.SetSelf(nodeName())
	nodes := node.NewRegistry(nodeName(), cfg.NodeConfigs())
	defer nodes.Close()
	manager.SetTransports(nodes)

	// Projects are the hub's assignment, recorded in the ledger from config
	// at every boot. Steve's own home directory is a project too — the one
	// the owner's DM works in — so nothing special-cases it downstream.
	projects := project.Open(book)
	declared := cfg.ProjectList()
	declared = append(declared, project.Project{ID: homeProjectID, Level: project.LevelRestricted, Home: project.Home{Path: cfg.Gateway.HomePath}})
	if err := projects.Declare(ctx, declared); err != nil {
		return fmt.Errorf("declare projects: %w", err)
	}
	for _, note := range cfg.Migrated {
		log.Printf("steve: config migrated: %s", note)
	}
	for _, g := range cfg.GrantList() {
		if _, err := projects.Grant(context.Background(), g.Project, g.Principal, g.Role, g.By); err != nil {
			return fmt.Errorf("grant %s in %s: %w", g.Principal, g.Project, err)
		}
	}
	startHubLaunch(context.Background(), cfg)
	self := hubAdvert(cfg)
	log.Printf("steve: hub %s — %s %v, %s/%s, level=%s, harnesses=%s, caps=%v",
		self.Node, self.Hostname, self.IPs, self.OS, self.Arch, cfg.HubLevel(), harnessSummary(self), self.Capabilities)
	for _, h := range self.Harnesses {
		if h.Missing != "" {
			log.Printf("steve: hub cannot run %s: %s", h.ID, h.Missing)
		}
	}
	for _, status := range nodes.Probe(ctx) {
		if !status.Up {
			return fmt.Errorf("node %q at %s unreachable: %s", status.Name, status.Addr, status.LastError)
		}
		log.Printf("steve: node %s up — %s/%s, level=%s, harnesses=%s, caps=%v",
			status.Name, status.Advert.OS, status.Advert.Arch, status.Level,
			harnessSummary(status.Advert), status.Advert.Capabilities)
		for _, h := range status.Advert.Harnesses {
			if h.Missing != "" {
				log.Printf("steve: node %s cannot run %s: %s", status.Name, h.ID, h.Missing)
			}
		}
	}

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
		at := harness.Placement{Node: selected.Node, Harness: selected.Harness}
		// An agent is probed in a project that lives where it runs. An agent
		// on a node no project is homed on has nowhere to open a session
		// yet, and that is reported rather than papered over.
		workspace, ok := probeWorkspace(declared, selected.Node)
		if !ok {
			log.Printf("steve: agent %s on %s: no project is homed there; session not probed", selected.ID, at)
			continue
		}
		session, err := manager.OpenSession(ctx, at, "", workspace, capabilities.MCPServers)
		if err != nil {
			return fmt.Errorf("agent %q session on %s: %w", selected.ID, at, err)
		}
		if err := manager.CloseSession(ctx, at, session.ID()); err != nil {
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

// top renders the read model in this terminal. It is a client of the running
// gateway's HTTP surface, not a second reader of the stores: one read model,
// two renderers, so the terminal and the browser cannot disagree.
func top(args []string) error {
	flags := flag.NewFlagSet("top", flag.ContinueOnError)
	url := flags.String("url", defaultReadModelURL, "read model URL of a running gateway")
	token := flags.String("token", "", "token, when the read model is not on loopback")
	refresh := flags.Duration("refresh", 5*time.Second, "redraw floor; changes also redraw immediately")
	once := flags.Bool("once", false, "print one frame and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	model := tui.New(tui.Config{URL: *url, Token: *token, Refresh: *refresh})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		fmt.Print(model.Once(ctx))
		return nil
	}
	return model.Run(ctx)
}

// dash prints the dashboard URL of a running gateway. The page is served by
// the gateway itself, so there is no second process to keep alive.
func dash(args []string) error {
	flags := flag.NewFlagSet("dash", flag.ContinueOnError)
	url := flags.String("url", defaultReadModelURL, "read model URL of a running gateway")
	token := flags.String("token", "", "token, when the read model is not on loopback")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url+"/state", nil)
	if err != nil {
		return err
	}
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("no gateway at %s — is `steve run` up? %w", *url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("read model at %s answered %s", *url, res.Status)
	}
	page := *url
	if *token != "" {
		page += "/?token=" + neturl.QueryEscape(*token)
	}
	fmt.Println(page)
	return nil
}

// defaultReadModelURL is where `steve run` puts the read model unless the
// config says otherwise.
const defaultReadModelURL = "http://127.0.0.1:7710"

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
	book, err := openLedger(cfg)
	if err != nil {
		return err
	}
	defer book.Close()
	store, err := state.OpenLedger(book, cfg.Gateway.StatePath)
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
	nodewire.SetSelf(nodeName())
	nodes := node.NewRegistry(nodeName(), cfg.NodeConfigs())
	defer nodes.Close()
	manager.SetTransports(nodes)

	// Projects are the hub's assignment, recorded in the ledger from config
	// at every boot. Steve's own home directory is a project too — the one
	// the owner's DM works in — so nothing special-cases it downstream.
	projects := project.Open(book)
	declared := cfg.ProjectList()
	declared = append(declared, project.Project{ID: homeProjectID, Level: project.LevelRestricted, Home: project.Home{Path: cfg.Gateway.HomePath}})
	if err := projects.Declare(context.Background(), declared); err != nil {
		return fmt.Errorf("declare projects: %w", err)
	}
	for _, note := range cfg.Migrated {
		log.Printf("steve: config migrated: %s", note)
	}

	// The roster is what turns "which agents exist" into "which agents can
	// run this right now", from live adverts rather than from config.
	fleet := roster.New(catalog)
	fleet.SetNodes(nodes)
	fleet.SetHubCapabilities(cfg.Gateway.Capabilities)
	startHubLaunch(context.Background(), cfg)
	fleet.SetHubAdvert(func() nodewire.Advert { return hubAdvert(cfg) })
	fleet.SetHubLevel(cfg.HubLevel())
	// What every harness was seen running, per machine: sessions report
	// it as they open, and a probe asks on purpose for the ones nobody
	// has used yet.
	seen := models.New()
	if err := seen.Persist(book.Document("models")); err != nil {
		return err
	}
	fleet.SetModels(seen)
	manager.SetObserver(func(at harness.Placement, s steveview.Settings) {
		seen.Observe(models.Observation{Node: at.Node, Harness: at.Harness, Current: s.Model, Available: s.Models, Version: s.Adapter, Source: "session", Selectors: selectorsOf(s.Options)})
	})
	prober := models.NewProber(manager, seen, func(ctx context.Context, node, dir string) error {
		if node == "" {
			return os.MkdirAll(dir, 0o700)
		}
		_, err := nodes.Exec(ctx, node, "", "mkdir -p '"+dir+"'")
		return err
	})
	probeDir := func(node string) string {
		if node == "" {
			return filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "probe")
		}
		for _, s := range nodes.Statuses() {
			if s.Name == node && s.Advert.StateDir != "" {
				return filepath.Join(s.Advert.StateDir, "probe")
			}
		}
		return ""
	}
	endpoints := func(ctx context.Context) []models.Endpoint {
		var eps []models.Endpoint
		known := map[string]bool{}
		for _, c := range fleet.All(ctx) {
			key := c.Node + "/" + c.Harness
			if !c.Eligible || known[key] {
				continue
			}
			dir := probeDir(c.Node)
			if dir == "" {
				continue
			}
			known[key] = true
			eps = append(eps, models.Endpoint{Node: c.Node, Harness: c.Harness, Workdir: dir})
		}
		return eps
	}
	fleet.SetHubSlots(cfg.HubSlots())
	fleet.SetNodeLevels(cfg.NodeLevels())
	fleet.SetNodeRegions(cfg.NodeRegions())
	nodes.SetHubLevel(string(cfg.HubLevel()))
	// Regions: this hub issues leases for its own machines and honours the
	// other hubs' for theirs.
	if cfg.Gateway.Region != "" {
		book.SetRegion(cfg.Gateway.Region)
	}
	for name, region := range cfg.Gateway.Regions {
		book.RegisterIssuer(name, ledger.NewHTTPIssuer(region.URL, region.Token))
	}
	if cfg.Gateway.IssuerAddr != "" {
		issuer := &http.Server{Addr: cfg.Gateway.IssuerAddr, Handler: ledger.IssuerHandler(book, cfg.Gateway.IssuerToken)}
		go func() {
			if err := issuer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("steve: lease issuer on %s: %v", cfg.Gateway.IssuerAddr, err)
			}
		}()
		defer issuer.Close()
		log.Printf("steve: issuing region %s leases on %s", book.Region(), cfg.Gateway.IssuerAddr)
	}

	coordinator := turn.New(
		catalog, store, assembler, manager, time.Duration(cfg.Gateway.PromptTimeout),
	)
	catalogText := i18n.New(i18n.FromDomain(cfg.Feishu.Domain))
	coordinator.SetIdentity(cfg.Feishu.OwnerOpenID, home.Dir{Path: cfg.Gateway.HomePath})
	coordinator.SetSkills(live)
	coordinator.SetProjects(projects, cfg.Gateway.DefaultProject, homeProjectID)
	// Memory: the home's MEMORY.md for the owner, one file per project,
	// every write locked and audited under the state directory.
	memoryDir := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "memory")
	memories := memory.NewService(memory.NewMarkdown(cfg.Gateway.HomePath, memoryDir), filepath.Join(memoryDir, "audit.jsonl"))
	coordinator.SetMemory(memories)
	// Attempts: every execution is leased and fenced. Anything left live by
	// a previous process is expired now, before a single turn runs.
	attempts := attempt.New(book)
	expired, err := attempts.Sweep(context.Background())
	if err != nil {
		return fmt.Errorf("sweep attempts: %w", err)
	}
	for _, r := range expired {
		log.Printf("steve: expired stale attempt %s", attempt.Describe(r))
	}
	go sweepAttempts(context.Background(), attempts)
	coordinator.SetAttempts(attempts)
	// Artifacts: every result is a commit in the project's shadow
	// repository on the hub, materialised wherever a step runs.
	artifacts := artifact.New(filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "artifacts"), book, projects, nodes)
	artifacts.Direct = cfg.Gateway.DirectTransfer
	coordinator.SetArtifacts(artifacts)
	// Side effects agents ask for are intents: claimed, journaled, and
	// blocked across attempts until a person resolves an unknown outcome.
	intents := intent.New(book)
	coordinator.SetIntents(intents)
	// A landing the previous process was cut off in is finished — or
	// stopped at a conflict — before any turn can touch the canonical.
	if recovered, err := artifacts.RecoverLandings(context.Background()); err != nil {
		return fmt.Errorf("recover landings: %w", err)
	} else {
		for _, l := range recovered {
			log.Printf("steve: recovered landing %s of %s into %s: %s", l.ID, l.Artifact, l.Project, l.State)
		}
	}
	tasks, err := task.OpenLedger(book, filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "tasks.json"))
	if err != nil {
		return fmt.Errorf("open tasks: %w", err)
	}
	tasks.SetBudget(cfg.Gateway.TaskMaxTurns, time.Duration(cfg.Gateway.TaskMaxElapsed))
	coordinator.SetTasks(tasks, nodeName())
	schedules, err := schedule.OpenLedger(book, filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "schedules.json"))
	if err != nil {
		return fmt.Errorf("open schedules: %w", err)
	}
	plans, err := plan.OpenLedger(book, filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "plans.json"))
	if err != nil {
		return fmt.Errorf("open plans: %w", err)
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

	// The supervisor is the plan side: a planner decides what should happen,
	// and the runtime holds everything that must be true regardless of who
	// planned it — placement against the live roster, the budget, the
	// verification rule, the recovery limit.
	stepRunner := exec.NewAgentRunner(manager, capabilitiesFor(assembler), fleet)
	// Workflow checkpoints are durable so a plan outlives the process that
	// started it; the ledger records which runs are open.
	workflowsDB := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "workflows.db")
	if err := gopactsqlite.Migrate(workflowsDB); err != nil {
		return fmt.Errorf("migrate workflow checkpoints: %w", err)
	}
	checkpoints, err := gopactsqlite.Open(workflowsDB)
	if err != nil {
		return fmt.Errorf("open workflow checkpoints: %w", err)
	}
	defer checkpoints.Close()
	supervisor := exec.NewSupervisor(
		choosePlanner(cfg, catalog, manager, artifacts),
		exec.Deps{
			Roster: fleet, Runner: stepRunner, Budget: taskBudget{tasks: tasks},
			// Verification runs where the work is: a command on the step's
			// node, or a second agent asked to check the first one's.
			Verifier:   exec.NewVerifiers(nodes, manager, fleet, artifacts),
			Workspaces: artifacts,
			Attempts:   attempts,
			Artifacts:  artifacts,
		},
		checkpoints,
	)
	supervisor.SetPlans(plans)
	supervisor.SetLedger(book, nodeName())
	coordinator.SetSupervisor(supervisor, plans, fleet)
	coordinator.SetRepair(nodes, nodes)
	coordinator.SetProber(func(ctx context.Context, node, harnessID string) error {
		dir := probeDir(node)
		if dir == "" {
			return fmt.Errorf("no state dir known for %s", nodewire.Place(node))
		}
		_, err := prober.Probe(ctx, models.Endpoint{Node: node, Harness: harnessID, Workdir: dir})
		return err
	}, func(ctx context.Context) []models.Result { return prober.ProbeAll(ctx, endpoints(ctx), true) })

	// One read model, two renderers. `steve top` and the browser are both
	// clients of this; neither reads the stores directly, so what the
	// operator sees in one place cannot contradict the other.
	// What each project's directory holds is asked of its machine on a
	// slow clock and kept: a page must not run git on every repaint.
	repos := &repoCache{projects: projects, nodes: nodes, hub: nodeName(), poke: make(chan struct{}, 1)}
	view := readmodel.New(readmodel.Sources{
		Hub: readmodel.Hub{
			Node: nodeName(), Started: time.Now(), Capabilities: cfg.Gateway.Capabilities,
			Level: string(cfg.HubLevel()),
		},
		HubAdvert: func() nodewire.Advert { return hubAdvert(cfg) },
		Repos:     repos.get, HomeProject: homeProjectID, DefaultProject: cfg.Gateway.DefaultProject,
		Models: seen,
		Roster: fleet, Nodes: nodes, Tasks: tasks, Plans: plans,
		Ledger:       readmodel.Ledger{Book: book, Attempts: attempts, Artifacts: artifacts, Projects: projects, Intents: intents},
		Schedules:    schedules,
		Observations: book.Document("observations"),
	})
	if err := view.LoadObservations(); err != nil {
		log.Printf("steve: observations: %v", err)
	}
	// A machine's abilities changing is history too: a tool that vanished
	// explains the placement that failed after it.
	nodes.SetDriftObserver(func(name string, changes []string) {
		view.Observe("node.manifest", name, name+": "+strings.Join(changes, "; "))
	})
	// Skills are the hub's to enable and every machine's to have: each
	// node gets the enabled set as a content-addressed bundle when it
	// connects and whenever the set changes, before harnesses restart.
	hubSkills = &skillShipper{nodes: nodes, live: live, observe: view.Observe}
	if _, err := hubSkills.pack(); err != nil {
		log.Printf("steve: skills could not be packed for nodes: %v", err)
	}
	live.After = func() error {
		hubSkills.shipAll(context.Background())
		return manager.Restart()
	}
	// Machines coming and going are history, not just log lines.
	nodes.SetObserver(func(s node.Status) {
		if s.Up {
			view.Observe("node.up", s.Name, fmt.Sprintf("%s connected: %s %s/%s, build %s", s.Name, s.Advert.Hostname, s.Advert.OS, s.Advert.Arch, s.Advert.BuildVersion))
			go hubSkills.ship(context.Background(), s.Name)
			// A machine that comes back may hold worktrees of attempts that
			// died with the connection; nothing else ever returns for them.
			if root := s.Advert.WorkspaceRoot; root != "" {
				go sweepWorktrees(context.Background(), artifacts, attempts, view, s.Name, root)
			}
			return
		}
		view.Observe("node.down", s.Name, fmt.Sprintf("%s disconnected: %s", s.Name, s.LastError))
	})
	stepRunner.SetObserver(func(req exec.StepRequest, p steveview.Progress) {
		view.StepProgress(req.TaskID, req.PlanID, req.StepID, req.Agent, req.Node, p)
	})
	dashboard, err := readmodel.NewServer(view, readmodel.ServerConfig{
		Addr: cfg.Gateway.ReadModelAddr, Token: cfg.Gateway.ReadModelToken,
	})
	if err != nil {
		return err
	}
	// The console: the owner acting from the page, through this same
	// coordinator. Notices anchored on the console stay on the page.
	cons := console.New(coordinator, cfg.Feishu.OwnerOpenID, view)
	if err := cons.Persist(book.Document("console")); err != nil {
		return err
	}
	cons.SetTitler(&conversationTitler{manager: manager, catalog: catalog, projects: projects, home: cfg.Gateway.HomePath})
	dashboard.SetConsole(cons)
	// A copy may only sit where the project's level admits; the store
	// asks the registry, which knows every machine's level.
	projects.Levels = func(node string) project.Level {
		level, err := nodes.Level(context.Background(), node)
		if err != nil {
			return ""
		}
		return project.Level(level)
	}
	admin := &fleetAdmin{cfg: cfg, path: *configPath, nodes: nodes, catalog: catalog, fleet: fleet, manager: manager, assembler: assembler, projects: projects, repos: repos, attempts: attempts, tasks: tasks, view: view,
		skills: live, shipper: hubSkills, coordinator: coordinator, homePath: cfg.Gateway.HomePath, memory: memories, artifacts: artifacts}
	dashboard.SetAdmin(admin)
	cons.SetInspector(admin)
	tasks.SetObserver(func(id string) { view.TaskChanged(id) })
	defer dashboard.Close()
	go func() {
		if err := dashboard.Serve(); err != nil {
			log.Printf("steve: read model: %v", err)
		}
	}()
	supervisor.Runs().Observe(view)
	log.Printf("steve: dashboard on %s  (steve top -url %s)", dashboard.URL(), dashboard.URL())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Dial the fleet now and keep redialing what is down. Without this the
	// registry only connects when something asks it to, so a hub that has
	// just started would report every node as down and refuse every
	// placement — describing its own ignorance rather than the fleet.
	nodes.Start(ctx)
	// What earlier processes and dropped connections left behind: the
	// hub's own orphaned worktrees now, queued landings from here on.
	go sweepWorktrees(ctx, artifacts, attempts, view, "", "")
	go sweepLandings(ctx, projects, artifacts, view)
	go repos.run(ctx)
	go sweepIdleTasks(ctx, tasks, attempts, view)
	// Discover models for whatever nobody has run yet. It is discovery,
	// not work: a session opened and closed, no prompt sent. Done off the
	// startup path so a slow adapter never delays the first message.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		for _, r := range prober.ProbeAll(ctx, endpoints(ctx), false) {
			if r.Err != nil {
				log.Printf("steve: probe %s/%s: %v", nodewire.Place(r.Endpoint.Node), r.Endpoint.Harness, r.Err)
				continue
			}
			log.Printf("steve: %s/%s runs %q, offers %v", nodewire.Place(r.Endpoint.Node), r.Endpoint.Harness, r.Current, r.Available)
		}
	}()

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
		coordinator.SetNodeEndpoints(nodes)
		// Delegation is the one way an agent reaches another: a child task
		// in the tree, funded from the caller's remainder, with its own
		// token. It is offered only when the messaging server exists,
		// because that is where the tool lives.
		delegation := delegate.New(tasks, fleet, manager, assembler, artifacts, nodeName())
		delegation.SetLedger(attempts, artifacts)
		delegation.SetGate(gate)
		delegation.SetEndpoints(nodes)
		delegation.MaxSilence = time.Duration(cfg.Gateway.PromptTimeout)
		delegation.SetObserver(func(c delegate.Child, p steveview.Progress) {
			where := c.Node
			if where == "" {
				where = nodeName() // the hub itself, named like any machine
			}
			info := readmodel.StepInfo{Goal: c.Goal, State: c.State, Since: c.Since.UTC().Format(time.RFC3339),
				Elapsed: c.Elapsed.Round(time.Second).String(), Answer: c.Answer, Refs: c.Refs}
			if c.State != "running" && c.Attempt != "" {
				if changes, err := admin.Changes(context.Background(), c.Attempt); err == nil && changes != nil {
					info.Attempt, info.Files = changes.Attempt, changes.Files
				}
			}
			view.DelegateProgress(c.Task, c.Agent, where, info, p)
		})
		gate.SetDelegator(delegation)
		// Remote agents call a loopback port on their own machine; the node
		// forwards it back here over the connection it already holds, so the
		// messaging server never has to leave 127.0.0.1.
		nodes.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", gate.Addr())
		})
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
	gate.SetIntents(intent.ForAgents{S: intents, Attempts: attempts})
	// The platform's own questions — where am I, where are the projects —
	// are answered by the coordinator, live.
	gate.SetInformer(coordinatorInformer{c: coordinator})
	gate.SetFleeter(fleetTools{admin: admin, view: view})
	gate.SetMemorizer(coordinator)
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
	channel.SetJournal(book.Journal())
	if gate != nil {
		gate.BindChannel(console.Sender{Feishu: channel, Console: cons})
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
	coordinator.ResumePlans(context.Background())
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

// harnessSummary renders a node's runtimes for one log line, marking the
// ones it cannot actually start.
func harnessSummary(advert nodewire.Advert) string {
	if len(advert.Harnesses) == 0 {
		return "none"
	}
	names := make([]string, 0, len(advert.Harnesses))
	for _, h := range advert.Harnesses {
		if h.Missing != "" {
			names = append(names, withSlots(h)+"(missing)")
			continue
		}
		names = append(names, withSlots(h))
	}
	return strings.Join(names, ",")
}

func withSlots(h nodewire.Harness) string {
	if h.Slots > 0 {
		return fmt.Sprintf("%s(%d)", h.ID, h.Slots)
	}
	return h.ID
}

// hubGeneration identifies this hub process for its own snapshots;
// hubSequence counts them.
var (
	hubGeneration = time.Now().Unix()
	hubSequence   atomic.Int64
)

// hubAdvert describes the hub's own machine the way a node's advert
// describes a node: the same harness check against this PATH, the same
// identity, so the fleet has one shape for every machine.
func hubAdvert(cfg *config.Config) nodewire.Advert {
	configMu.RLock()
	defer configMu.RUnlock()
	specs := make(map[string]node.HarnessSpec, len(cfg.Harnesses))
	for id, h := range cfg.Harnesses {
		specs[id] = node.HarnessSpec{Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir, Slots: h.Slots}
	}
	adv := node.Advertise(nodeName(), specs, cfg.Gateway.Capabilities)
	mcp := make(map[string]node.MCPSpec, len(cfg.MCPServers))
	for id, m := range cfg.MCPServers {
		mcp[id] = node.MCPSpec{Type: m.Type, Command: m.Command, Args: m.Args, URL: m.URL}
	}
	entries, known := hubSkills.entries()
	adv.Snapshot = node.Snapshot(nodeName(), hubGeneration, hubSequence.Add(1), node.Observe{Harnesses: specs, Tools: cfg.Gateway.Tools, MCP: mcp, Declares: cfg.Gateway.Declares, Tags: cfg.Gateway.Capabilities, Launch: hubLaunch.Lookup, Skills: entries, SkillsKnown: known})
	adv.Features = nodewire.Features()
	adv.OwnSkills = node.OwnSkills(5 * time.Minute)
	adv.StateDir = filepath.Dir(cfg.Gateway.StatePath)
	adv.Health = node.CheckHealth("", adv.StateDir)
	return adv
}

// fleetAdmin adds machines and agents from the page: the running
// registry and catalog take them at once, and the config file records
// them so a restart keeps them. A new machine gets a token of its own
// and one command to run.
type fleetAdmin struct {
	mu      sync.Mutex
	cfg     *config.Config
	path    string
	nodes   *node.Registry
	catalog *agent.Catalog
	fleet   *roster.Roster
	// projects is the ledger's project store; repos the hub's view of what
	// each project's directory holds.
	projects *project.Store
	repos    *repoCache
	// attempts is where a copy's lock is taken before it is forgotten.
	attempts *attempt.Service
	tasks    *task.Store
	view     *readmodel.Model
	// artifacts is the project snapshots, for a turn's changes.
	artifacts *artifact.Store
	// skills is the live map of what agents are handed, shipper what each
	// machine holds of it; coordinator holds the lock a change needs.
	skills      *skills.Live
	shipper     *skillShipper
	coordinator *turn.Coordinator
	// homePath is Steve's own directory: who it is, who the owner is,
	// what it remembers.
	homePath string
	// memory is what Steve remembers, for the page to read and edit.
	memory *memory.Service
	// probes remembers what each MCP deployment answered when last
	// probed, by "<machine>/<name>", with the shape it had then; a
	// deployment whose shape moved is shown as stale. provenance
	// remembers where an installed or adopted deployment came from.
	probeMu    sync.Mutex
	probes     map[string]mcpProbeEntry
	provenance map[string]string
	// manager and assembler take the hub machine's own harness and MCP
	// changes at runtime.
	manager   *harness.Manager
	assembler *capability.Assembler
	// hubURL is how the last page that added a machine reached the hub;
	// the bootstrap script fetches the binary from there.
	hubURL string
}

// configMu guards the loaded configuration: the admin rewrites parts of
// it from the page while hubAdvert and the launch probe read it.
var configMu sync.RWMutex

// NodeSettings reads what a machine offers: the hub's own from its
// config, a node's from the node.
func (a *fleetAdmin) NodeSettings(ctx context.Context, name string) (nodewire.Settings, error) {
	if name == nodeName() {
		return a.hubSettings(), nil
	}
	return a.nodes.Settings(ctx, name)
}

// SetNodeSettings rewrites what a machine offers and answers what is in
// force: on the hub, the config file and the running manager, assembler,
// roster and launch probe; on a node, the node itself.
func (a *fleetAdmin) SetNodeSettings(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	if name != nodeName() {
		return a.nodes.Configure(ctx, name, set)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.setNodeSettingsLocked(ctx, name, set)
}

// setNodeSettingsLocked is SetNodeSettings for the hub with a.mu held.
func (a *fleetAdmin) setNodeSettingsLocked(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error) {
	_ = ctx
	if len(set.Harnesses) == 0 {
		return nodewire.Settings{}, fmt.Errorf("hub 至少要有一个 AI 工具")
	}
	harnesses := make(map[string]config.Harness, len(set.Harnesses))
	for id, h := range set.Harnesses {
		if !nameShape.MatchString(strings.ToLower(id)) || strings.TrimSpace(h.Command) == "" {
			return nodewire.Settings{}, fmt.Errorf("AI 工具 %q 需要一个合法的名字和启动命令", id)
		}
		item := config.Harness{Command: h.Command, Args: h.Args, ProcessDir: h.ProcessDir, Env: h.Env}
		configMu.RLock()
		if old, ok := a.cfg.Harnesses[id]; ok {
			item.Permission = old.Permission
		}
		configMu.RUnlock()
		harnesses[id] = item
	}
	servers := make(map[string]config.MCPServer, len(set.MCPServers))
	for id, m := range set.MCPServers {
		if !nameShape.MatchString(strings.ToLower(id)) {
			return nodewire.Settings{}, fmt.Errorf("MCP 服务器 %q 的名字不合法", id)
		}
		switch m.Type {
		case "", "stdio":
			if strings.TrimSpace(m.Command) == "" {
				return nodewire.Settings{}, fmt.Errorf("MCP 服务器 %q 需要启动命令", id)
			}
			m.Type = "stdio"
		case "http", "sse":
			if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
				return nodewire.Settings{}, fmt.Errorf("MCP 服务器 %q 需要 http(s) 地址", id)
			}
		default:
			return nodewire.Settings{}, fmt.Errorf("MCP 服务器 %q：不认识的类型 %q", id, m.Type)
		}
		servers[id] = config.MCPServer{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	for _, d := range set.Declares {
		if !strings.Contains(d, ":") {
			return nodewire.Settings{}, fmt.Errorf("声明 %q 要写成 kind:id，如 network:office", d)
		}
	}
	configMu.Lock()
	// Agents keep naming harnesses that exist.
	for id, ag := range a.cfg.Agents {
		if ag.Node == "" {
			if _, ok := harnesses[ag.Harness]; !ok {
				configMu.Unlock()
				return nodewire.Settings{}, fmt.Errorf("Agent %s 还在用 AI 工具 %s，不能删", id, ag.Harness)
			}
			for _, srv := range ag.MCPServers {
				if _, ok := servers[srv]; !ok {
					configMu.Unlock()
					return nodewire.Settings{}, fmt.Errorf("Agent %s 还在用 MCP 服务器 %s，不能删", id, srv)
				}
			}
		}
	}
	old := *a.cfg
	a.cfg.Harnesses = harnesses
	a.cfg.MCPServers = servers
	a.cfg.Gateway.Tools = append([]string(nil), set.Tools...)
	a.cfg.Gateway.Declares = append([]string(nil), set.Declares...)
	a.cfg.Gateway.Capabilities = append([]string(nil), set.Capabilities...)
	if err := config.Save(a.path, a.cfg); err != nil {
		a.cfg.Harnesses, a.cfg.MCPServers, a.cfg.Gateway = old.Harnesses, old.MCPServers, old.Gateway
		configMu.Unlock()
		return nodewire.Settings{}, fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	configMu.Unlock()
	// The running pieces follow the file.
	for id, h := range harnesses {
		if err := a.manager.Set(id, harness.Config{Command: h.Command, Args: h.Args, ProcessDir: h.ProcessDir, Env: h.Env, Permission: h.Permission}); err != nil {
			log.Printf("steve: harness %s: %v", id, err)
		}
	}
	for id := range old.Harnesses {
		if _, keep := harnesses[id]; !keep {
			a.manager.Remove(id)
		}
	}
	caps := make(map[string]capability.MCPServer, len(servers))
	for id, m := range servers {
		caps[id] = capability.MCPServer{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	a.assembler.SetServers(caps)
	a.fleet.SetHubCapabilities(set.Capabilities)
	hubLaunch.Wake()
	log.Printf("steve: hub settings applied from the page: %d harnesses, %d tools, %d mcp, %d declares, %d tags",
		len(harnesses), len(set.Tools), len(servers), len(set.Declares), len(set.Capabilities))
	return a.hubSettings(), nil
}

func (a *fleetAdmin) hubSettings() nodewire.Settings {
	configMu.RLock()
	defer configMu.RUnlock()
	out := nodewire.Settings{Harnesses: map[string]nodewire.HarnessSetting{}, Tools: append([]string{}, a.cfg.Gateway.Tools...),
		MCPServers: map[string]nodewire.MCPSetting{}, Declares: append([]string{}, a.cfg.Gateway.Declares...), Capabilities: append([]string{}, a.cfg.Gateway.Capabilities...)}
	for id, h := range a.cfg.Harnesses {
		out.Harnesses[id] = nodewire.HarnessSetting{Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir}
	}
	for id, m := range a.cfg.MCPServers {
		out.MCPServers[id] = nodewire.MCPSetting{Type: m.Type, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers}
	}
	return out
}

var nameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (a *fleetAdmin) AddNode(_ context.Context, req readmodel.AddNodeRequest) (readmodel.AddNodeResult, error) {
	name := strings.TrimSpace(req.Name)
	if !nameShape.MatchString(name) {
		return readmodel.AddNodeResult{}, fmt.Errorf("机器名只能是小写字母、数字、点、下划线、连字符，如 node-c")
	}
	addr := strings.TrimSpace(req.Addr)
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return readmodel.AddNodeResult{}, fmt.Errorf("地址要写成 ip:端口，如 10.0.0.5:7701")
	}
	level := project.Level(strings.TrimSpace(req.Level)).OrDefault()
	if _, ok := map[project.Level]bool{project.LevelPublic: true, project.LevelInternal: true, project.LevelRestricted: true, project.LevelSealed: true}[level]; !ok {
		return readmodel.AddNodeResult{}, fmt.Errorf("数据等级只能是 public / internal / restricted / sealed")
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return readmodel.AddNodeResult{}, err
	}
	token := hex.EncodeToString(raw[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hubURL = req.HubURL
	if _, exists := a.cfg.Nodes[name]; exists {
		return readmodel.AddNodeResult{}, fmt.Errorf("机器 %s 已经存在", name)
	}
	if name == nodeName() {
		return readmodel.AddNodeResult{}, fmt.Errorf("%s 是 hub 自己", name)
	}
	if a.cfg.Nodes == nil {
		a.cfg.Nodes = map[string]config.Node{}
	}
	a.cfg.Nodes[name] = config.Node{Addr: addr, Token: token, Level: string(level)}
	if err := config.Save(a.path, a.cfg); err != nil {
		delete(a.cfg.Nodes, name)
		return readmodel.AddNodeResult{}, fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	a.nodes.Add(name, node.Config{Addr: addr, Token: token, Level: string(level)})
	a.fleet.SetNodeLevels(a.cfg.NodeLevels())
	a.fleet.SetNodeRegions(a.cfg.NodeRegions())
	log.Printf("steve: machine %s added (%s, %s); waiting for it to come up", name, addr, level)
	out := readmodel.AddNodeResult{Name: name, Token: token,
		Command: fmt.Sprintf("curl -fsSL '%s/bootstrap/%s?token=%s' | bash -l", req.HubURL, name, token)}
	if a.cfg.Gateway.NodeBinary == "" {
		out.Note = "hub 没有配置 gateway.node_binary，脚本不会下载 steve-node：先把它放到那台机器的 ~/steve-bin/steve-node。"
	}
	return out, nil
}

// RemoveNode forgets a machine. Nothing may still live on it: an agent
// placed there, a project homed there or with a copy there, keep it.
func (a *fleetAdmin) RemoveNode(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == nodeName() || name == "hub" {
		return fmt.Errorf("%q 是 hub 自己，不能移除", name)
	}
	if a.catalog != nil {
		for _, ag := range a.catalog.List() {
			if ag.Node == name {
				return fmt.Errorf("Agent %s 还在 %s 上；先把它移到别的机器或删掉", ag.ID, name)
			}
		}
	}
	if a.projects != nil {
		if list, err := a.projects.List(ctx); err == nil {
			for _, p := range list {
				for _, ws := range p.Workspaces() {
					if ws.Node == name {
						kind := "主目录"
						if ws.Kind == project.KindCopy {
							kind = "副本"
						}
						return fmt.Errorf("项目 %s 的%s还在 %s 上；先移除它", p.ID, kind, name)
					}
				}
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	configMu.Lock()
	saved, inConfig := a.cfg.Nodes[name]
	if !inConfig {
		configMu.Unlock()
		return fmt.Errorf("没有叫 %q 的机器", name)
	}
	delete(a.cfg.Nodes, name)
	if err := config.Save(a.path, a.cfg); err != nil {
		a.cfg.Nodes[name] = saved
		configMu.Unlock()
		return fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	levels, regions := a.cfg.NodeLevels(), a.cfg.NodeRegions()
	configMu.Unlock()
	a.nodes.Remove(name)
	a.fleet.SetNodeLevels(levels)
	a.fleet.SetNodeRegions(regions)
	log.Printf("steve: machine %s removed", name)
	return nil
}

// AddProject declares a project: the ledger takes it at once, the config
// file records it, and its directory is inspected right away.
func (a *fleetAdmin) AddProject(ctx context.Context, req readmodel.AddProjectRequest) error {
	id := strings.TrimSpace(req.ID)
	if !nameShape.MatchString(id) {
		return fmt.Errorf("项目名只能是小写字母、数字、点、下划线、连字符")
	}
	if id == homeProjectID {
		return fmt.Errorf("%s 是 Steve 自己的家，不能再声明", id)
	}
	path := strings.TrimSpace(req.Path)
	if path == "" || !(strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~")) {
		return fmt.Errorf("目录要写绝对路径，如 /home/me/work 或 ~/work")
	}
	level := project.Level(strings.TrimSpace(req.Level)).OrDefault()
	if _, ok := map[project.Level]bool{project.LevelPublic: true, project.LevelInternal: true, project.LevelRestricted: true, project.LevelSealed: true}[level]; !ok {
		return fmt.Errorf("数据等级只能是 public / internal / restricted / sealed")
	}
	repo := project.RepoMode(strings.TrimSpace(req.Repo))
	if repo == "" {
		repo = project.RepoInPlace
	}
	if repo != project.RepoInPlace && repo != project.RepoIsolated {
		return fmt.Errorf("工作方式只能是 inplace 或 isolated")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	configMu.Lock()
	if req.Node != "" {
		if _, ok := a.cfg.Nodes[req.Node]; !ok {
			configMu.Unlock()
			return fmt.Errorf("没有叫 %q 的机器", req.Node)
		}
	}
	if _, exists := a.cfg.Projects[id]; exists {
		configMu.Unlock()
		return fmt.Errorf("项目 %s 已经存在", id)
	}
	if a.cfg.Projects == nil {
		a.cfg.Projects = map[string]config.Project{}
	}
	if req.Node == "" && strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	// A directory belongs to one workspace, of one project: the store
	// knows every home and every copy, and judges nesting the same way
	// for both.
	if other, taken, err := a.projects.Conflict(ctx, req.Node, path); err != nil {
		configMu.Unlock()
		return err
	} else if taken {
		configMu.Unlock()
		return fmt.Errorf("目录与项目 %s 的工作区（%s）重叠：一个目录只能属于一个工作区，也不能套在另一个工作区的目录里", other.Project, other.Path)
	}
	a.cfg.Projects[id] = config.Project{Home: config.ProjectHome{Node: req.Node, Path: path}, Level: string(level), Repo: string(repo)}
	declared := config.Config{Projects: map[string]config.Project{id: a.cfg.Projects[id]}}
	if err := config.Save(a.path, a.cfg); err != nil {
		delete(a.cfg.Projects, id)
		configMu.Unlock()
		return fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	configMu.Unlock()
	if err := a.projects.Declare(ctx, declared.ProjectList()); err != nil {
		return err
	}
	if a.repos != nil {
		a.repos.wake()
	}
	log.Printf("steve: project %s declared (%s:%s, %s, %s)", id, orHubName(req.Node), path, level, repo)
	return nil
}

// UpdateAgent replaces the editable part of an agent; its aliases,
// prompt, skills and default flag stay as the file has them.
func (a *fleetAdmin) UpdateAgent(_ context.Context, id string, spec readmodel.AgentSpec) error {
	id = strings.ToLower(strings.TrimSpace(id))
	a.mu.Lock()
	defer a.mu.Unlock()
	configMu.Lock()
	defer configMu.Unlock()
	item, ok := a.cfg.Agents[id]
	if !ok {
		return fmt.Errorf("没有叫 %q 的 Agent", id)
	}
	if _, ok := a.cfg.Harnesses[spec.Harness]; !ok {
		return fmt.Errorf("hub 的 harnesses 里没有 %q", spec.Harness)
	}
	if spec.Node != "" {
		if _, ok := a.cfg.Nodes[spec.Node]; !ok {
			return fmt.Errorf("没有叫 %q 的机器", spec.Node)
		}
	}
	if err := ability.ValidateText(spec.Requires); err != nil {
		return fmt.Errorf("运行条件：%w", err)
	}
	if spec.Node == "" {
		for _, srv := range spec.MCPServers {
			if _, ok := a.cfg.MCPServers[srv]; !ok {
				return fmt.Errorf("hub 上没有 MCP 服务器 %q；在 hub 机器的配置里加，或把 Agent 放到有它的机器上", srv)
			}
		}
	}
	old := item
	item.Harness, item.Node, item.Model = spec.Harness, spec.Node, strings.TrimSpace(spec.Model)
	item.About = strings.TrimSpace(spec.About)
	item.Options = map[string]string{}
	for k, v := range spec.Options {
		if k = strings.TrimSpace(k); k != "" && strings.TrimSpace(v) != "" {
			item.Options[k] = strings.TrimSpace(v)
		}
	}
	if len(item.Options) == 0 {
		item.Options = nil
	}
	item.Requires = append([]string{}, spec.Requires...)
	item.MCPServers = append([]string{}, spec.MCPServers...)
	toAgent := func(it config.Agent) agent.Config {
		return agent.Config{Harness: it.Harness, Node: it.Node, Model: it.Model, Options: it.Options, About: it.About, Requires: it.Requires, Aliases: it.Aliases,
			SystemPrompt: it.SystemPrompt, Skills: it.Skills, MCPServers: it.MCPServers, Default: it.Default}
	}
	if err := a.catalog.Set(id, toAgent(item)); err != nil {
		return err
	}
	a.cfg.Agents[id] = item
	if err := config.Save(a.path, a.cfg); err != nil {
		a.cfg.Agents[id] = old
		_ = a.catalog.Set(id, toAgent(old))
		return fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	log.Printf("steve: agent %s updated (%s on %s, model %q)", id, item.Harness, orHubName(item.Node), item.Model)
	return nil
}

// RemoveAgent forgets an agent; the default one stays.
func (a *fleetAdmin) RemoveAgent(_ context.Context, id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	a.mu.Lock()
	defer a.mu.Unlock()
	configMu.Lock()
	defer configMu.Unlock()
	item, ok := a.cfg.Agents[id]
	if !ok {
		return fmt.Errorf("没有叫 %q 的 Agent", id)
	}
	if item.Default {
		return fmt.Errorf("%s 是默认 Agent，不能删；先在配置里换一个默认", id)
	}
	if err := a.catalog.Remove(id); err != nil {
		return err
	}
	delete(a.cfg.Agents, id)
	if err := config.Save(a.path, a.cfg); err != nil {
		a.cfg.Agents[id] = item
		return fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	log.Printf("steve: agent %s removed", id)
	return nil
}

// RemoveProject retires a project: gone from the ledger and from the
// config file. Steve's home and the default project stay.
// nodeKey is the registry's name for a machine the page named: the hub
// is "" inside, whatever it is called outside.
func (a *fleetAdmin) nodeKey(name string) string {
	if name == "" || name == "hub" || name == nodewire.Place("") {
		return ""
	}
	return name
}

// inspect asks a machine what a directory holds.
func (a *fleetAdmin) inspect(ctx context.Context, nodeKey, path string) []nodewire.Repo {
	if nodeKey == "" {
		return node.InspectRepos(ctx, path)
	}
	ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	repos, err := a.nodes.Inspect(ictx, nodeKey, path)
	if err != nil {
		return nil
	}
	return repos
}

// AddWorkspace gives a project a copy on a machine. Adopting takes a
// directory that is there as it is. Cloning needs somewhere to clone
// from — the project's external remote, or the remote of the single
// repository its home directory is — and a path with nothing at it; the
// copy is recorded as provisioning and the clone runs behind the reply,
// in a staging directory beside the destination that becomes it only
// when the clone is whole.
func (a *fleetAdmin) AddWorkspace(ctx context.Context, projectID string, req readmodel.AddWorkspaceRequest) error {
	nodeKey := a.nodeKey(req.Node)
	path := strings.TrimSpace(req.Path)
	if path == "" {
		return errors.New("目录不能为空")
	}
	if nodeKey == "" && strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if nodeKey != "" {
		configMu.Lock()
		_, known := a.cfg.Nodes[nodeKey]
		configMu.Unlock()
		if !known {
			return fmt.Errorf("没有叫 %q 的机器", req.Node)
		}
	}
	p, ok, err := a.projects.Get(ctx, projectID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("没有叫 %q 的项目", projectID)
	}
	found := a.inspect(ctx, nodeKey, path)
	missing := len(found) == 1 && found[0].Missing
	c := project.Copy{Node: nodeKey, Path: path, Origin: project.OriginAdopted, By: "console", State: project.CopyReady}
	if req.Origin == "clone" {
		source := a.cloneSource(p)
		if source == "" {
			return fmt.Errorf("项目 %s 没有可以克隆的来源：主目录不是单个 git 仓库或没有 remote，也没配 external_remote。可以先在机器上放好目录，再认领它", p.ID)
		}
		if !missing {
			return fmt.Errorf("%s 上已经有 %s 了；要用它就认领，要克隆就换一个还不存在的目录", nodewire.Place(nodeKey), path)
		}
		c.Origin, c.Source, c.State = project.OriginCloned, source, project.CopyProvisioning
	} else if missing {
		return fmt.Errorf("%s 上没有目录 %s；认领的目录要已经存在，不存在就选克隆", nodewire.Place(nodeKey), path)
	}
	if _, err := a.projects.SetCopy(ctx, projectID, c); err != nil {
		return err
	}
	if err := a.saveWorkspaces(ctx, projectID); err != nil {
		_ = a.projects.DeleteCopy(ctx, projectID, nodeKey)
		return err
	}
	if c.State == project.CopyProvisioning {
		go a.clone(projectID, nodeKey, path, c)
	} else if a.repos != nil {
		a.repos.wake()
	}
	return nil
}

// cloneSource is what a copy of the project is cloned from: the external
// remote it declares, else the remote of the repository its home is.
func (a *fleetAdmin) cloneSource(p project.Project) string {
	if p.ExternalRemote != "" {
		return p.ExternalRemote
	}
	if a.repos == nil {
		return ""
	}
	for _, r := range a.repos.get(p.Canonical().ID) {
		if r.Path == "." && r.Remote != "" {
			return r.Remote
		}
	}
	return ""
}

// clone runs the clone on the machine and records how it went.
func (a *fleetAdmin) clone(projectID, nodeKey, path string, c project.Copy) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := a.nodes.Exec(ctx, nodeKey, "", cloneScript(path, c.Source))
	if err != nil {
		c.State, c.Error = project.CopyFailed, clip(strings.TrimSpace(out+"\n"+err.Error()), 400)
		log.Printf("steve: clone %s onto %s: %v", projectID, nodewire.Place(nodeKey), err)
	} else {
		c.State, c.Error = project.CopyReady, ""
		log.Printf("steve: cloned %s onto %s at %s", projectID, nodewire.Place(nodeKey), path)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.projects.SetCopy(ctx, projectID, c); err != nil {
		log.Printf("steve: record clone of %s: %v", projectID, err)
	}
	if a.repos != nil {
		a.repos.wake()
	}
}

// cloneScript clones source into dest through a staging directory beside
// it, so a clone that dies leaves nothing at dest. Git may not ask for
// credentials: there is no one at the terminal to answer.
func cloneScript(dest, source string) string {
	return strings.Join([]string{
		"set -e",
		"export GIT_TERMINAL_PROMPT=0",
		"dest=" + shellQuote(dest),
		"src=" + shellQuote(source),
		`parent=$(dirname "$dest")`,
		`mkdir -p "$parent"`,
		`tmp=$(mktemp -d "$parent/.steve-clone.XXXXXX")`,
		`trap 'rm -rf "$tmp"' EXIT`,
		`git clone --quiet -- "$src" "$tmp/repo" 2>&1`,
		`mv "$tmp/repo" "$dest"`,
	}, "\n")
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// saveWorkspaces writes the project's copies to the config file, from
// the ledger's record of them.
func (a *fleetAdmin) saveWorkspaces(ctx context.Context, projectID string) error {
	p, ok, err := a.projects.Get(ctx, projectID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("没有叫 %q 的项目", projectID)
	}
	configMu.Lock()
	defer configMu.Unlock()
	item, inConfig := a.cfg.Projects[projectID]
	if !inConfig {
		return nil
	}
	saved := item.Workspaces
	item.Workspaces = nil
	for _, ws := range p.Workspaces() {
		if ws.Kind == project.KindCopy {
			item.Workspaces = append(item.Workspaces, config.ProjectWorkspace{Node: ws.Node, Path: ws.Path})
		}
	}
	a.cfg.Projects[projectID] = item
	if err := config.Save(a.path, a.cfg); err != nil {
		item.Workspaces = saved
		a.cfg.Projects[projectID] = item
		return fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	return nil
}

// RemoveWorkspace forgets a project's copy. It takes the copy's lock
// first, so a turn cannot start in a directory the hub is forgetting.
func (a *fleetAdmin) RemoveWorkspace(ctx context.Context, projectID, nodeName string) error {
	nodeKey := a.nodeKey(nodeName)
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok, err := a.projects.Get(ctx, projectID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("没有叫 %q 的项目", projectID)
	}
	if _, has := p.CopyOn(nodeKey); !has {
		return fmt.Errorf("项目 %s 在 %s 上没有副本", projectID, nodewire.Place(nodeKey))
	}
	if a.attempts != nil {
		release, err := a.attempts.Hold(ctx, a.fleet.RegionOf(nodeKey), project.CopyID(projectID, nodeKey), "console")
		if err != nil {
			var busy attempt.Busy
			if errors.As(err, &busy) {
				return fmt.Errorf("%w：副本里有回合在跑（%s），等它结束再移除", readmodel.ErrBusy, busy.Holder)
			}
			return err
		}
		defer release()
	}
	if err := a.projects.DeleteCopy(ctx, projectID, nodeKey); err != nil {
		return err
	}
	if err := a.saveWorkspaces(ctx, projectID); err != nil {
		return err
	}
	if a.repos != nil {
		a.repos.wake()
	}
	return nil
}

// Skills is the skills page: what is found where, what is on, who pins
// what by path, and which machines hold the current bundle.
func (a *fleetAdmin) Skills(ctx context.Context) (readmodel.SkillsView, error) {
	if a.skills == nil || a.skills.Map == nil {
		return readmodel.SkillsView{}, errors.New("技能没有配置")
	}
	view := readmodel.SkillsView{Fingerprint: a.skills.Map.Fingerprint(), SearchPaths: a.skills.Map.SearchPaths(), BuiltinRoot: a.skills.Map.BuiltinRootPath(), Skills: []readmodel.SkillView{}, Nodes: []readmodel.SkillNode{}, Sources: []readmodel.SkillSource{}}
	// A skill from a source resolves into its clone; that, not the name,
	// says which source it came from — a skill of the same name from
	// the user's directory or the shipped set is not the source's.
	type clone struct{ slug, root, dir string }
	var clones []clone
	for _, src := range a.skills.Map.Sources() {
		view.Sources = append(view.Sources, skillSource(src))
		dir := src.Dir
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		clones = append(clones, clone{src.Slug, src.Root, dir})
	}
	if view.SearchPaths == nil {
		view.SearchPaths = []string{}
	}
	enabled, err := a.skills.Map.Enabled()
	if err != nil {
		return view, err
	}
	on := map[string]bool{}
	for _, ref := range enabled {
		on[ref.Name] = true
	}
	available, err := a.skills.Map.Available()
	if err != nil {
		return view, err
	}
	// Who asks for a skill by path: an agent's or a project's pinned
	// skill sources are directories, matched against the skill's.
	byPath := map[string]*readmodel.SkillView{}
	for _, ref := range available {
		d := skills.Describe(ref.Path)
		item := readmodel.SkillView{Name: ref.Name, Path: ref.Path, Root: filepath.Dir(ref.Path), Title: d.Title, Description: d.Description, Enabled: on[ref.Name], Builtin: view.BuiltinRoot != "" && filepath.Dir(ref.Path) == view.BuiltinRoot, Agents: []string{}, Projects: []string{}}
		for _, c := range clones {
			if strings.HasPrefix(ref.Path, c.dir+string(filepath.Separator)) {
				item.Source, item.Root = c.slug, c.root
			}
		}
		view.Skills = append(view.Skills, item)
		byPath[filepath.Clean(ref.Path)] = &view.Skills[len(view.Skills)-1]
	}
	if a.catalog != nil {
		for _, ag := range a.catalog.List() {
			for _, src := range ag.Skills {
				if item, ok := byPath[filepath.Clean(src)]; ok {
					item.Agents = append(item.Agents, ag.ID)
				}
			}
		}
	}
	if a.projects != nil {
		if list, err := a.projects.List(ctx); err == nil {
			for _, p := range list {
				for _, src := range p.Skills {
					if item, ok := byPath[filepath.Clean(src)]; ok {
						item.Projects = append(item.Projects, p.ID)
					}
				}
			}
		}
	}
	want := a.shipper.hash()
	for _, name := range a.nodes.Names() {
		item := readmodel.SkillNode{Name: name}
		if adv, err := a.nodes.Advert(ctx, name); err == nil {
			item.Up = true
			item.Takes = slices.Contains(adv.Features, nodewire.FeatureSkills)
			item.Synced = want != "" && adv.Skills == want
		}
		view.Nodes = append(view.Nodes, item)
	}
	return view, nil
}

// SkillContent is one skill's SKILL.md.
func (a *fleetAdmin) SkillContent(_ context.Context, name string) (readmodel.SkillDoc, error) {
	if a.skills == nil || a.skills.Map == nil {
		return readmodel.SkillDoc{}, errors.New("技能没有配置")
	}
	available, err := a.skills.Map.Available()
	if err != nil {
		return readmodel.SkillDoc{}, err
	}
	for _, ref := range available {
		if ref.Name == name {
			content, err := skills.Content(ref.Path)
			if err != nil {
				return readmodel.SkillDoc{}, err
			}
			return readmodel.SkillDoc{Name: ref.Name, Path: ref.Path, Content: content}, nil
		}
	}
	return readmodel.SkillDoc{}, fmt.Errorf("没有叫 %q 的技能", name)
}

// MachineSkills is what every machine last said its AI tools have of
// their own: the hub's own scan, and each node's advert as the registry
// holds it. Nothing is asked here; the refresh loop asks every minute,
// and RefreshMachineSkills asks now.
func (a *fleetAdmin) MachineSkills(ctx context.Context) []readmodel.MachineSkills {
	have := map[string]bool{}
	if a.skills != nil && a.skills.Map != nil {
		if avail, err := a.skills.Map.Available(); err == nil {
			for _, ref := range avail {
				have[ref.Name] = true
			}
		}
	}
	found := func(name string, hub bool, own []nodewire.OwnSkill, err error) readmodel.MachineSkills {
		item := readmodel.MachineSkills{Name: nodewire.Place(name), Hub: hub, Skills: []readmodel.FoundSkill{}}
		if err != nil {
			item.Error = clip(strings.TrimSpace(err.Error()), 200)
			return item
		}
		item.Up = true
		for _, f := range own {
			item.Skills = append(item.Skills, readmodel.FoundSkill{Name: f.Name, Path: f.Path, Title: f.Title, Description: f.Description, Loaded: have[f.Name]})
		}
		return item
	}
	out := []readmodel.MachineSkills{found("", true, node.OwnSkills(5*time.Minute), nil)}
	for _, name := range a.nodes.Names() {
		adv, err := a.nodes.Advert(ctx, name)
		out = append(out, found(name, false, adv.OwnSkills, err))
	}
	return out
}

// RefreshMachineSkills asks every machine to look again, at once, and
// returns what they said; one that is down or slow says so.
func (a *fleetAdmin) RefreshMachineSkills(ctx context.Context) []readmodel.MachineSkills {
	ownSkillsRescan()
	names := a.nodes.Names()
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			_, _ = a.nodes.Refresh(rctx, name)
		}(name)
	}
	wg.Wait()
	return a.MachineSkills(ctx)
}

func ownSkillsRescan() { node.OwnSkills(0) }

// ImportSkill loads a machine's skill onto the hub, into the owner's own
// skills directory, where it is a hub skill like any other — not enabled
// until the owner says so.
func (a *fleetAdmin) ImportSkill(ctx context.Context, nodeName, path string) (string, error) {
	if a.skills == nil || a.skills.Map == nil {
		return "", errors.New("技能没有配置")
	}
	path = strings.TrimSpace(path)
	if path == "" || !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("目录 %q 不是绝对路径", path)
	}
	name := filepath.Base(path)
	dest := filepath.Join(a.skills.Map.UserDir(), name)
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("hub 上已经有叫 %s 的技能（%s）", name, dest)
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	encoded, err := a.nodes.Exec(sctx, a.nodeKey(nodeName), "", skills.ImportScript(path))
	if err != nil {
		return "", fmt.Errorf("从 %s 取 %s：%s", nodewire.Place(a.nodeKey(nodeName)), path, clip(strings.TrimSpace(err.Error()), 200))
	}
	if err := skills.UnpackImport(encoded, dest); err != nil {
		return "", err
	}
	return name, nil
}

// coordinatorInformer answers the platform MCP server's questions from
// the coordinator, in the server's own shapes.
type coordinatorInformer struct{ c *turn.Coordinator }

func (i coordinatorInformer) Context(ctx context.Context, conversationID, agentID string) (agentmcp.ContextInfo, error) {
	w, err := i.c.Where(ctx, conversationID, agentID)
	if err != nil {
		return agentmcp.ContextInfo{}, err
	}
	return agentmcp.ContextInfo{Agent: w.Agent, Node: w.Node, Harness: w.Harness, Model: w.Model, Mode: w.Mode, Project: w.Project, ProjectNode: w.ProjectNode, Level: w.Level, Repo: w.Repo,
		Workspace: w.Workspace, WorkspaceKind: w.WorkspaceKind, Why: w.Why, Task: w.Task, Turns: w.Turns, MaxTurns: w.MaxTurns, Elapsed: w.Elapsed, MaxElapsed: w.MaxElapsed, MCPServers: w.MCPServers, Skills: w.Skills}, nil
}

func (i coordinatorInformer) Projects(ctx context.Context, conversationID, agentID string) (string, error) {
	return i.c.WhereProjects(ctx, conversationID, agentID)
}

// fleetTools answers the platform MCP server's fleet questions and
// changes from the admin and the read model.
type fleetTools struct {
	admin *fleetAdmin
	view  *readmodel.Model
}

func (f fleetTools) Nodes(ctx context.Context) (string, error) {
	snap := f.view.Snapshot(ctx)
	type harness struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Model string `json:"model,omitempty"`
		Why   string `json:"why,omitempty"`
	}
	type machine struct {
		Name      string    `json:"name"`
		Role      string    `json:"role"`
		Up        bool      `json:"up"`
		Since     string    `json:"since,omitempty"`
		LastError string    `json:"last_error,omitempty"`
		Version   string    `json:"version,omitempty"`
		Level     string    `json:"level,omitempty"`
		OS        string    `json:"os,omitempty"`
		DiskFree  string    `json:"disk_free,omitempty"`
		Load      float64   `json:"load1,omitempty"`
		Worktrees int       `json:"worktrees,omitempty"`
		Harnesses []harness `json:"harnesses"`
		MCP       []string  `json:"mcp_servers"`
		OwnSkills int       `json:"own_skills"`
		Agents    []string  `json:"agents"`
	}
	agentsOn := map[string][]string{}
	for _, ag := range snap.Agents {
		agentsOn[ag.Node] = append(agentsOn[ag.Node], ag.ID)
	}
	out := make([]machine, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		m := machine{Name: n.Name, Role: n.Role, Up: n.Up, Version: n.Version, Level: n.Level, OS: n.OS, LastError: n.LastError, Harnesses: []harness{}, MCP: []string{}, Agents: agentsOn[n.Name]}
		if !n.Since.IsZero() {
			m.Since = n.Since.UTC().Format(time.RFC3339)
		}
		if n.Health != nil && n.Health.DiskTotal > 0 {
			m.DiskFree = fmt.Sprintf("%.0f GB", float64(n.Health.DiskFree)/(1<<30))
			m.Load, m.Worktrees = n.Health.Load1, n.Health.Worktrees
		}
		for _, h := range n.Harnesses {
			state := "ready"
			if h.Missing != "" {
				state = "missing"
			}
			m.Harnesses = append(m.Harnesses, harness{ID: h.ID, State: state, Model: h.Model, Why: h.Missing})
		}
		if n.Snapshot != nil {
			for _, c := range n.Snapshot.Offers {
				if c.Kind == ability.MCP {
					m.MCP = append(m.MCP, c.ID)
				}
			}
		}
		if adv, err := f.admin.advertOf(ctx, f.admin.nodeKey(n.Name)); err == nil {
			m.OwnSkills = len(adv.OwnSkills)
		}
		if m.Agents == nil {
			m.Agents = []string{}
		}
		sort.Strings(m.MCP)
		out = append(out, m)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	return string(raw), err
}

func (f fleetTools) AddNode(ctx context.Context, name, addr, level, hubURL string) (string, error) {
	if hubURL == "" {
		f.admin.mu.Lock()
		hubURL = f.admin.hubURL
		f.admin.mu.Unlock()
	}
	if hubURL == "" {
		return "", errors.New("需要 hub_url：那台机器怎么访问 hub 的控制台，如 http://10.0.0.1:7710")
	}
	res, err := f.admin.AddNode(ctx, readmodel.AddNodeRequest{Name: name, Addr: addr, Level: level, HubURL: hubURL})
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("机器 %s 已登记（%s）。在那台机器上以登录 shell 跑这一条，它会装好 steve-node 并连上来：\n\n%s", res.Name, addr, res.Command)
	if res.Note != "" {
		text += "\n\n" + res.Note
	}
	return text, nil
}

func (f fleetTools) RemoveNode(ctx context.Context, name string) error {
	return f.admin.RemoveNode(ctx, name)
}

func (f fleetTools) RefreshNode(ctx context.Context, name string) (string, error) {
	key := f.admin.nodeKey(name)
	if key == "" {
		return "", errors.New("hub 自己不用刷新，它的申报是现算的")
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	adv, err := f.admin.nodes.Refresh(rctx, key)
	if err != nil {
		return "", err
	}
	summary := map[string]any{"name": name, "version": adv.BuildVersion, "harnesses": len(adv.Harnesses), "own_skills": len(adv.OwnSkills), "own_mcp": len(adv.OwnMCP), "features": adv.Features}
	if adv.Health != nil && adv.Health.DiskTotal > 0 {
		summary["disk_free_gb"] = adv.Health.DiskFree >> 30
		summary["load1"] = adv.Health.Load1
	}
	raw, err := json.MarshalIndent(summary, "", "  ")
	return string(raw), err
}

func skillSource(s skills.Source) readmodel.SkillSource {
	out := readmodel.SkillSource{Slug: s.Slug, URL: s.URL, Ref: s.Ref, Subdir: s.Subdir, Root: s.Root, Head: s.Head, FetchedAt: s.FetchedAt, Skills: s.Skills, Error: s.Error}
	if out.Skills == nil {
		out.Skills = []string{}
	}
	return out
}

// AddSkillSource installs a git repository of skills; nothing is
// enabled by it, so nothing restarts.
func (a *fleetAdmin) AddSkillSource(ctx context.Context, spec string) (readmodel.SkillSource, error) {
	if a.skills == nil || a.skills.Map == nil {
		return readmodel.SkillSource{}, errors.New("技能没有配置")
	}
	src, err := a.skills.AddSource(ctx, spec)
	if err != nil {
		return readmodel.SkillSource{}, err
	}
	return skillSource(src), nil
}

// UpdateSkillSources fetches every source again; the text of enabled
// skills may change, so it takes the lock and restarts the AI tools.
func (a *fleetAdmin) UpdateSkillSources(ctx context.Context) ([]readmodel.SkillSource, error) {
	var out []readmodel.SkillSource
	err := a.withSkillsLock(func() error {
		updated, err := a.skills.UpdateSources(ctx)
		for _, s := range updated {
			out = append(out, skillSource(s))
		}
		return err
	})
	return out, err
}

// RemoveSkillSource forgets a source; skills enabled from it go with it.
func (a *fleetAdmin) RemoveSkillSource(_ context.Context, slug string) error {
	return a.withSkillsLock(func() error { return a.skills.RemoveSource(slug) })
}

// withSkillsLock runs a change to the skills the way the chat verb does:
// not while a turn runs, since the change restarts the AI tools.
func (a *fleetAdmin) withSkillsLock(op func() error) error {
	if a.skills == nil || a.skills.Map == nil {
		return errors.New("技能没有配置")
	}
	if a.coordinator != nil {
		release, ok := a.coordinator.SkillsLock()
		if !ok {
			return fmt.Errorf("%w：有回合在跑，改技能会重启 AI 工具，等它结束再改", readmodel.ErrBusy)
		}
		defer release()
	}
	return op()
}

// SetSkill turns a skill on or off for every agent. Agents in flight
// keep their session; the next session opens with the new set, and the
// changed fingerprint tells the owner to /new.
func (a *fleetAdmin) SetSkill(_ context.Context, name string, enabled bool) error {
	return a.withSkillsLock(func() error {
		if enabled {
			return a.skills.Enable(name)
		}
		return a.skills.Disable(name)
	})
}

// AddSkillPath adds a directory to look for skills in.
func (a *fleetAdmin) AddSkillPath(_ context.Context, path string) error {
	if a.skills == nil || a.skills.Map == nil {
		return errors.New("技能没有配置")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("目录不能为空")
	}
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return fmt.Errorf("hub 上没有目录 %s", path)
	}
	return a.skills.AddPath(path)
}

// RemoveSkillPath stops looking in a directory; skills enabled from it
// go with it, so it takes the lock.
func (a *fleetAdmin) RemoveSkillPath(_ context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("目录不能为空")
	}
	return a.withSkillsLock(func() error { return a.skills.RemovePath(path) })
}

type mcpProbeEntry struct {
	view  readmodel.MCPProbeView
	shape string
}

// mcpShape digests the public shape of a deployment: transport, command,
// arguments, URL, and the names of its variables and headers. A probe
// taken under one shape is stale under another.
func mcpShape(s nodewire.MCPSetting) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", s.Type, s.Command, strings.Join(s.Args, "\x01"), s.URL)
	for _, k := range sortedKeys(s.Env) {
		fmt.Fprintf(h, "e:%s\x00", k)
	}
	for _, k := range sortedKeys(s.Headers) {
		fmt.Fprintf(h, "h:%s\x00", k)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reservedMCP are names the platform gives its own session servers.
func reservedMCP(name string) bool { return name == "feishu" || strings.HasPrefix(name, "steve") }

// mcpSettingsOf reads a machine's MCP settings: the hub's from config,
// a node's from the node (values included, as the config stream returns
// them; the page is handed only the keys).
func (a *fleetAdmin) mcpSettingsOf(ctx context.Context, nodeKey string) (map[string]nodewire.MCPSetting, error) {
	if nodeKey == "" {
		return a.hubSettings().MCPServers, nil
	}
	set, err := a.nodes.Settings(ctx, nodeKey)
	if err != nil {
		return nil, err
	}
	return set.MCPServers, nil
}

// advertOf is what a machine last said about itself; the hub's is made now.
func (a *fleetAdmin) advertOf(ctx context.Context, nodeKey string) (nodewire.Advert, error) {
	if nodeKey == "" {
		return hubAdvert(a.cfg), nil
	}
	return a.nodes.Advert(ctx, nodeKey)
}

// MCP is the MCP page: every deployment on every machine with who
// attaches and the last probe, the platform's session servers, and
// what each machine's coding agents configured themselves.
func (a *fleetAdmin) MCP(ctx context.Context) (readmodel.MCPView, error) {
	view := readmodel.MCPView{Deployments: []readmodel.MCPDeployment{}, Platform: []readmodel.MCPPlatform{}, Machines: []readmodel.MCPMachine{}}
	attach := map[string][]string{}
	if a.catalog != nil {
		for _, ag := range a.catalog.List() {
			for _, m := range ag.MCPServers {
				key := nodewire.Place(ag.Node) + "/" + m
				attach[key] = append(attach[key], ag.ID)
			}
		}
	}
	counts := map[string]int{}
	a.probeMu.Lock()
	probes := make(map[string]mcpProbeEntry, len(a.probes))
	for k, v := range a.probes {
		probes[k] = v
	}
	provenance := make(map[string]string, len(a.provenance))
	for k, v := range a.provenance {
		provenance[k] = v
	}
	a.probeMu.Unlock()
	for _, nodeKey := range append([]string{""}, a.nodes.Names()...) {
		place := nodewire.Place(nodeKey)
		adv, advErr := a.advertOf(ctx, nodeKey)
		resolvable := map[string]*bool{}
		if adv.Snapshot != nil {
			for _, c := range adv.Snapshot.Offers {
				if c.Kind != ability.MCP {
					continue
				}
				for _, e := range c.Evidence {
					ok := e.OK
					resolvable[c.ID] = &ok
				}
			}
		}
		settings, err := a.mcpSettingsOf(ctx, nodeKey)
		if err != nil && adv.Snapshot != nil {
			// The machine cannot be asked right now: what it last said
			// it had is still worth listing.
			settings = map[string]nodewire.MCPSetting{}
			for _, c := range adv.Snapshot.Offers {
				if c.Kind == ability.MCP {
					settings[c.ID] = nodewire.MCPSetting{Type: c.Attrs["transport"]}
				}
			}
		}
		for _, name := range sortedMCP(settings) {
			set := settings[name]
			d := readmodel.MCPDeployment{Node: place, Name: name, Type: set.Type, Command: set.Command, Args: set.Args, URL: set.URL, EnvKeys: sortedKeys(set.Env), HeaderKeys: sortedKeys(set.Headers), Agents: attach[place+"/"+name], Resolvable: resolvable[name], Provenance: provenance[place+"/"+name]}
			if d.Agents == nil {
				d.Agents = []string{}
			}
			if p, ok := probes[place+"/"+name]; ok {
				pv := p.view
				if p.shape != "" && p.shape != mcpShape(set) {
					pv.Stale = true
				}
				d.Probe = &pv
			}
			counts[name]++
			view.Deployments = append(view.Deployments, d)
		}
		m := readmodel.MCPMachine{Name: place, Hub: nodeKey == "", Up: advErr == nil, Own: []readmodel.MCPOwn{}}
		own := adv.OwnMCP
		if nodeKey == "" {
			own = node.OwnMCP(5 * time.Minute)
		} else if advErr == nil && !nodewire.HasFeature(adv.Features, nodewire.FeatureMCPProbe) {
			m.Unsupported = true
		}
		for _, o := range own {
			_, adopted := settings[o.Name]
			m.Own = append(m.Own, readmodel.MCPOwn{Name: o.Name, Source: o.Source, Scope: o.Scope, Type: o.Type, Command: o.Command, Args: o.Args, URL: o.URL, EnvKeys: o.EnvKeys, HeaderKeys: o.HeaderKeys, Adopted: adopted})
		}
		view.Machines = append(view.Machines, m)
	}
	for i := range view.Deployments {
		view.Deployments[i].SameNameElsewhere = counts[view.Deployments[i].Name] > 1
	}
	tools := []readmodel.PlatformTool{}
	for _, t := range agentmcp.PlatformTools(true) {
		tools = append(tools, readmodel.PlatformTool{Name: t.Name, Description: t.Description})
	}
	view.Platform = append(view.Platform, readmodel.MCPPlatform{Name: agentmcp.ServerName, Description: "hub 为每个会话现场生成：你在哪（steve_context）、项目在哪（steve_projects）、做法（steve_help）、进度卡，以及（接了委派时）看机器、委派、等结果", Tools: tools})
	return view, nil
}

func sortedMCP(m map[string]nodewire.MCPSetting) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ProbeMCP asks a deployment what tools it offers — on the hub directly,
// on a node through the node, which binds it the way a session would —
// and remembers the answer. A failed probe keeps the last good tool
// list, marked stale, beside the error.
func (a *fleetAdmin) ProbeMCP(ctx context.Context, machine, name string) (readmodel.MCPProbeView, error) {
	nodeKey := a.nodeKey(machine)
	place := nodewire.Place(nodeKey)
	settings, err := a.mcpSettingsOf(ctx, nodeKey)
	if err != nil {
		return readmodel.MCPProbeView{}, err
	}
	set, ok := settings[name]
	if !ok {
		return readmodel.MCPProbeView{}, fmt.Errorf("%s 上没有叫 %q 的 MCP 服务器", place, name)
	}
	view := readmodel.MCPProbeView{At: time.Now().UTC(), Tools: []nodewire.MCPTool{}}
	pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var reply nodewire.MCPProbeReply
	if nodeKey == "" {
		result, perr := mcpprobe.Probe(pctx, mcpprobe.Server{Type: set.Type, Command: set.Command, Args: set.Args, Env: set.Env, URL: set.URL, Headers: set.Headers})
		if perr != nil {
			reply.Error = perr.Error()
		} else {
			reply = nodewire.MCPProbeReply{ServerName: result.ServerName, ServerVersion: result.ServerVersion, Protocol: result.Protocol, Digest: result.Digest, ElapsedMS: result.Elapsed.Milliseconds()}
			for _, t := range result.Tools {
				reply.Tools = append(reply.Tools, nodewire.MCPTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
			}
		}
	} else {
		reply, err = a.nodes.MCPProbe(pctx, nodeKey, name)
		if err != nil {
			reply.Error = err.Error()
		}
	}
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if a.probes == nil {
		a.probes = map[string]mcpProbeEntry{}
	}
	if reply.Error != "" {
		view.Error = clip(reply.Error, 600)
		if prev, had := a.probes[place+"/"+name]; had && prev.view.OK {
			view.Tools, view.Digest, view.Stale = prev.view.Tools, prev.view.Digest, true
			view.ServerName, view.ServerVersion = prev.view.ServerName, prev.view.ServerVersion
		}
	} else {
		view.OK = true
		view.Tools, view.Digest, view.ServerName, view.ServerVersion, view.Protocol = reply.Tools, reply.Digest, reply.ServerName, reply.ServerVersion, reply.Protocol
		if view.Tools == nil {
			view.Tools = []nodewire.MCPTool{}
		}
	}
	a.probes[place+"/"+name] = mcpProbeEntry{view: view, shape: mcpShape(set)}
	return view, nil
}

func (a *fleetAdmin) remember(place, name, where string) {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if a.provenance == nil {
		a.provenance = map[string]string{}
	}
	if where == "" {
		delete(a.provenance, place+"/"+name)
		delete(a.probes, place+"/"+name)
		return
	}
	a.provenance[place+"/"+name] = where
}

// AdoptMCP copies a coding agent's own MCP server into a machine's
// settings, on that machine: the hub's from its own files, a node's by
// telling the node to. Values never pass through here.
func (a *fleetAdmin) AdoptMCP(ctx context.Context, machine, source, name string) error {
	nodeKey := a.nodeKey(machine)
	if reservedMCP(name) {
		return fmt.Errorf("%q 是平台自己用的名字，换个名字再纳入", name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if nodeKey != "" {
		if _, err := a.nodes.AdoptMCP(ctx, nodeKey, source, name); err != nil {
			return err
		}
		a.remember(nodewire.Place(nodeKey), name, "adopted:"+source)
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	full, ok := mcpscan.Lookup(home, source, name)
	if !ok {
		return fmt.Errorf("hub 这个用户的 %s 配置里没有 %q", source, name)
	}
	set := a.hubSettings()
	if _, exists := set.MCPServers[name]; exists {
		return fmt.Errorf("hub 上已经有叫 %q 的 MCP 服务器；先删掉它，或换个名字", name)
	}
	if set.MCPServers == nil {
		set.MCPServers = map[string]nodewire.MCPSetting{}
	}
	set.MCPServers[name] = nodewire.MCPSetting{Type: full.Type, Command: full.Command, Args: full.Args, Env: full.Env, URL: full.URL, Headers: full.Headers}
	if _, err := a.setNodeSettingsLocked(ctx, nodeName(), set); err != nil {
		return err
	}
	a.remember(nodewire.Place(""), name, "adopted:"+source)
	node.OwnMCP(0)
	return nil
}

// RemoveMCP drops a deployment from a machine's settings. An agent on
// that machine still naming it keeps it.
func (a *fleetAdmin) RemoveMCP(ctx context.Context, machine, name string) error {
	nodeKey := a.nodeKey(machine)
	place := nodewire.Place(nodeKey)
	if a.catalog != nil {
		for _, ag := range a.catalog.List() {
			if nodewire.Place(ag.Node) == place && slices.Contains(ag.MCPServers, name) {
				return fmt.Errorf("Agent %s 还在用 %s 上的 %q；先在资源页把它从 Agent 的 MCP 列表里去掉", ag.ID, place, name)
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var set nodewire.Settings
	var err error
	if nodeKey == "" {
		set = a.hubSettings()
	} else if set, err = a.nodes.Settings(ctx, nodeKey); err != nil {
		return err
	}
	if _, ok := set.MCPServers[name]; !ok {
		return fmt.Errorf("%s 上没有叫 %q 的 MCP 服务器", place, name)
	}
	delete(set.MCPServers, name)
	if nodeKey == "" {
		_, err = a.setNodeSettingsLocked(ctx, nodeName(), set)
	} else {
		_, err = a.nodes.Configure(ctx, nodeKey, set)
	}
	if err != nil {
		return err
	}
	a.remember(place, name, "")
	return nil
}

// SearchMCPRegistry asks the official registry.
func (a *fleetAdmin) SearchMCPRegistry(ctx context.Context, q string) ([]readmodel.MCPRegistryEntry, error) {
	entries, err := mcpregistry.Search(ctx, q, 20)
	if err != nil {
		return nil, err
	}
	out := make([]readmodel.MCPRegistryEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, registryEntryView(e))
	}
	return out, nil
}

func registryEntryView(e mcpregistry.Entry) readmodel.MCPRegistryEntry {
	out := readmodel.MCPRegistryEntry{Name: e.Name, Description: e.Description, Version: e.Version, Repository: e.Repository, Packages: []readmodel.MCPRegistryPackage{}, Remotes: []readmodel.MCPRegistryRemote{}}
	envs := func(in []mcpregistry.EnvVar) []readmodel.MCPRegistryEnv {
		vs := make([]readmodel.MCPRegistryEnv, 0, len(in))
		for _, v := range in {
			vs = append(vs, readmodel.MCPRegistryEnv{Name: v.Name, Description: v.Description, Required: v.Required, Secret: v.Secret, Default: v.Default})
		}
		return vs
	}
	for _, p := range e.Packages {
		out.Packages = append(out.Packages, readmodel.MCPRegistryPackage{RegistryType: p.RegistryType, Identifier: p.Identifier, Version: p.Version, RuntimeHint: p.RuntimeHint, Transport: p.Transport, Needs: p.Needs, Env: envs(p.Env)})
	}
	for _, r := range e.Remotes {
		out.Remotes = append(out.Remotes, readmodel.MCPRegistryRemote{Type: r.Type, URL: r.URL, Headers: envs(r.Headers)})
	}
	return out
}

// InstallMCP puts a registry entry on a machine: the chosen package
// becomes a command the machine must be able to run, the chosen remote
// an address that may not point inside; what the entry asked for is
// filled from what the owner typed and sent to that machine once.
func (a *fleetAdmin) InstallMCP(ctx context.Context, req readmodel.InstallMCPRequest) error {
	nodeKey := a.nodeKey(req.Node)
	place := nodewire.Place(nodeKey)
	name := strings.TrimSpace(req.Name)
	if !nameShape.MatchString(strings.ToLower(name)) {
		return fmt.Errorf("名字 %q 不合规：小写字母、数字、点、下划线、连字符", name)
	}
	if reservedMCP(name) {
		return fmt.Errorf("%q 是平台自己用的名字", name)
	}
	entries, err := mcpregistry.Search(ctx, req.Entry, 50)
	if err != nil {
		return err
	}
	var entry *mcpregistry.Entry
	for i := range entries {
		if entries[i].Name == req.Entry {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("注册表里找不到 %q", req.Entry)
	}
	var setting mcpregistry.Setting
	var wants []mcpregistry.EnvVar
	var provenance string
	switch {
	case req.Package != nil:
		if *req.Package < 0 || *req.Package >= len(entry.Packages) {
			return errors.New("没有这个包")
		}
		pkg := entry.Packages[*req.Package]
		setting, err = mcpregistry.Plan(pkg)
		if err != nil {
			return err
		}
		wants = pkg.Env
		if pkg.Needs != "" && !a.machineHasTool(ctx, nodeKey, pkg.Needs) {
			return fmt.Errorf("%s 上没有 %s，装不了这个包；先在那台机器上装好 %s", place, pkg.Needs, pkg.Needs)
		}
		provenance = "registry:" + entry.Name + " " + pkg.RegistryType + ":" + pkg.Identifier
		if pkg.Version != "" {
			provenance += "@" + pkg.Version
		}
		setting.Env = map[string]string{}
		for _, v := range wants {
			val := req.Values[v.Name]
			if val == "" {
				val = v.Default
			}
			if val == "" {
				if v.Required {
					return fmt.Errorf("%s 是必填的", v.Name)
				}
				continue
			}
			setting.Env[v.Name] = val
		}
	case req.Remote != nil:
		if *req.Remote < 0 || *req.Remote >= len(entry.Remotes) {
			return errors.New("没有这个远端")
		}
		remote := entry.Remotes[*req.Remote]
		setting, err = mcpregistry.PlanRemote(remote)
		if err != nil {
			return err
		}
		if err := refusePrivate(ctx, setting.URL); err != nil {
			return err
		}
		wants = remote.Headers
		provenance = "registry:" + entry.Name + " remote"
		setting.Headers = map[string]string{}
		for _, v := range wants {
			val := req.Values[v.Name]
			if val == "" {
				val = v.Default
			}
			if val == "" {
				if v.Required {
					return fmt.Errorf("%s 是必填的", v.Name)
				}
				continue
			}
			setting.Headers[v.Name] = val
		}
	default:
		return errors.New("要选一个包或一个远端")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var set nodewire.Settings
	if nodeKey == "" {
		set = a.hubSettings()
	} else if set, err = a.nodes.Settings(ctx, nodeKey); err != nil {
		return err
	}
	if _, exists := set.MCPServers[name]; exists {
		return fmt.Errorf("%s 上已经有叫 %q 的 MCP 服务器", place, name)
	}
	if set.MCPServers == nil {
		set.MCPServers = map[string]nodewire.MCPSetting{}
	}
	set.MCPServers[name] = nodewire.MCPSetting{Type: setting.Type, Command: setting.Command, Args: setting.Args, Env: setting.Env, URL: setting.URL, Headers: setting.Headers}
	if nodeKey == "" {
		_, err = a.setNodeSettingsLocked(ctx, nodeName(), set)
	} else {
		_, err = a.nodes.Configure(ctx, nodeKey, set)
	}
	if err != nil {
		return err
	}
	a.remember(place, name, provenance)
	return nil
}

// machineHasTool says whether a machine's snapshot offers a command.
func (a *fleetAdmin) machineHasTool(ctx context.Context, nodeKey, tool string) bool {
	adv, err := a.advertOf(ctx, nodeKey)
	if err != nil || adv.Snapshot == nil {
		return false
	}
	for _, c := range adv.Snapshot.Offers {
		if c.Kind == ability.Tool && c.ID == tool {
			return true
		}
	}
	return false
}

// refusePrivate keeps a remote from pointing at this machine, a private
// network, a link-local address or a cloud metadata service: a
// registry entry is a stranger's, and the machine's loopback proxy
// would carry the owner's headers wherever it says.
func refusePrivate(ctx context.Context, raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("地址 %q 不合规", raw)
	}
	host := u.Hostname()
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(lctx, host)
		if err != nil {
			return fmt.Errorf("解析 %s 失败：%v", host, err)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.Equal(net.ParseIP("169.254.169.254")) || (ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64) {
			return fmt.Errorf("%s 指向本机或内网（%s），默认不允许从注册表装到这样的地址", host, ip)
		}
	}
	return nil
}

// Home is Steve's own directory as the page shows it.
func (a *fleetAdmin) Home(_ context.Context) (readmodel.HomeView, error) {
	if a.homePath == "" {
		return readmodel.HomeView{}, errors.New("没有配置档案目录（gateway.home_path）")
	}
	files, err := home.Files(a.homePath)
	if err != nil {
		return readmodel.HomeView{}, err
	}
	view := readmodel.HomeView{Path: a.homePath, TotalBudget: home.BudgetTotal, Files: []readmodel.HomeFile{}, Warnings: []string{}}
	for _, f := range files {
		if f.Name == home.FileMemory && a.memory != nil && !f.Missing {
			if text, err := a.memory.Text(context.Background(), memory.Global); err == nil {
				f.Text = text
			}
		}
		view.Files = append(view.Files, readmodel.HomeFile{Name: f.Name, Text: f.Text, Bytes: len([]byte(f.Text)), Budget: f.Budget, Template: f.Template, Missing: f.Missing})
	}
	dir := home.Dir{Path: a.homePath}
	if snap, err := dir.Load(home.ModeOwner); err == nil {
		view.OwnerBytes = len([]byte(snap.Identity))
		view.Warnings = append(view.Warnings, snap.Warnings...)
	} else {
		view.Warnings = append(view.Warnings, err.Error())
	}
	if snap, err := dir.Load(home.ModeGuest); err == nil {
		view.GuestBytes = len([]byte(snap.Identity))
	}
	view.Projects = []readmodel.ProjectMemory{}
	if a.memory != nil && a.projects != nil {
		view.Audit = a.memory.AuditPath()
		list, err := a.projects.List(context.Background())
		if err != nil {
			view.Warnings = append(view.Warnings, err.Error())
		}
		for _, p := range list {
			if p.ID == homeProjectID {
				continue
			}
			scope := memory.ProjectScope(p.ID)
			text, err := a.memory.Text(context.Background(), scope)
			if err != nil {
				view.Warnings = append(view.Warnings, p.ID+": "+err.Error())
				continue
			}
			items, _ := a.memory.List(context.Background(), scope)
			view.Projects = append(view.Projects, readmodel.ProjectMemory{ID: p.ID, Path: a.memory.Where(scope), Text: text, Bytes: len([]byte(text)), Budget: memory.Budget(scope), Facts: len(items)})
		}
	}
	return view, nil
}

func (a *fleetAdmin) SetTaskMeta(ctx context.Context, taskID string, patch readmodel.TaskMetaPatch) (readmodel.Task, error) {
	if a.tasks == nil || a.view == nil {
		return readmodel.Task{}, errors.New("tasks are not wired")
	}
	if _, err := a.tasks.SetMeta(taskID, task.MetaPatch{
		Title: patch.Title, Priority: patch.Priority, Labels: patch.Labels, Archived: patch.Archived,
	}); err != nil {
		return readmodel.Task{}, err
	}
	for _, t := range a.view.Snapshot(ctx).Tasks {
		if t.ID == taskID {
			return t, nil
		}
	}
	return readmodel.Task{}, fmt.Errorf("task %s not found", taskID)
}

// ProjectOf is the project a conversation works in, for the console's
// quote boundary.
func (a *fleetAdmin) ProjectOf(ctx context.Context, conversation string) string {
	if a.coordinator == nil {
		return ""
	}
	return a.coordinator.ProjectOf(ctx, conversation)
}

// Selectors are what an agent offers in a thread, with what is chosen.
func (a *fleetAdmin) Selectors(ctx context.Context, conversation, agent string) (readmodel.Selectors, error) {
	if a.coordinator == nil {
		return readmodel.Selectors{}, errors.New("coordinator is not wired")
	}
	sel, err := a.coordinator.Selectors(ctx, conversation, agent)
	if err != nil {
		return readmodel.Selectors{}, err
	}
	return readmodel.Selectors{Model: sel.Model, Models: sel.Models, Options: sel.Options, Preferred: a.coordinator.Preferences(conversation, agent)}, nil
}

// SetPreferences records the owner's choices for an agent in a thread.
func (a *fleetAdmin) SetPreferences(ctx context.Context, conversation, agent string, patch map[string]string) error {
	if a.coordinator == nil {
		return errors.New("coordinator is not wired")
	}
	return a.coordinator.SetPreferences(ctx, conversation, agent, patch)
}

// TaskAttempts are a task's attempts from the ledger, newest first.
func (a *fleetAdmin) TaskAttempts(ctx context.Context, taskID string) ([]readmodel.AttemptView, error) {
	if a.attempts == nil {
		return nil, errors.New("attempts are not wired")
	}
	records, err := a.attempts.ForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]readmodel.AttemptView, 0, len(records))
	for _, r := range records {
		v := readmodel.AttemptView{ID: r.ID, Kind: string(r.Kind), State: string(r.State), Agent: r.Agent, Node: r.Node, Harness: r.Harness,
			Workspace: r.Workspace.Path, Base: r.Base, Error: r.Error, StartedAt: r.StartedAt, EndedAt: r.EndedAt}
		if r.Result != nil {
			v.Artifact, v.Summary = r.Result.Artifact, r.Result.Summary
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	// How many files each changed, for the newest few: one diff-tree each.
	for i := range out {
		if i >= 20 || a.artifacts == nil || out[i].Artifact == "" || out[i].Artifact == out[i].Base {
			continue
		}
		if changes, _, err := a.artifacts.Changes(ctx, records[0].Project, out[i].Base, out[i].Artifact); err == nil {
			out[i].Files = len(changes)
		}
	}
	return out, nil
}

// attemptSnapshot is the snapshot an attempt is browsed at: its result
// when it has one, else what it started from.
func (a *fleetAdmin) attemptSnapshot(ctx context.Context, attemptID string) (attempt.Record, string, string, error) {
	record, base, after, err := a.changeSnapshots(ctx, attemptID)
	if err != nil {
		return attempt.Record{}, "", "", err
	}
	if after != "" {
		return record, after, "result", nil
	}
	if base == "" {
		return attempt.Record{}, "", "", errors.New("this attempt has no snapshot to browse")
	}
	return record, base, "base", nil
}

// AttemptTree lists a directory of an attempt's snapshot.
func (a *fleetAdmin) AttemptTree(ctx context.Context, attemptID, dir string) (readmodel.TreeView, error) {
	record, commit, which, err := a.attemptSnapshot(ctx, attemptID)
	if err != nil {
		return readmodel.TreeView{}, err
	}
	entries, truncated, err := a.artifacts.Tree(ctx, record.Project, commit, dir)
	if err != nil {
		return readmodel.TreeView{}, err
	}
	return readmodel.TreeView{Attempt: attemptID, Commit: commit, Which: which, Dir: strings.Trim(dir, "/"), Entries: entries, Truncated: truncated}, nil
}

// AttemptFile reads one file of an attempt's snapshot.
func (a *fleetAdmin) AttemptFile(ctx context.Context, attemptID, path string) (readmodel.FileView, error) {
	record, commit, _, err := a.attemptSnapshot(ctx, attemptID)
	if err != nil {
		return readmodel.FileView{}, err
	}
	text, size, binary, truncated, err := a.artifacts.File(ctx, record.Project, commit, path)
	if err != nil {
		return readmodel.FileView{}, err
	}
	return readmodel.FileView{Attempt: attemptID, Commit: commit, Path: path, Text: text, Size: size, Binary: binary, Truncated: truncated}, nil
}

// changeSnapshots is the before and after of an attempt, when it captured
// one: nothing changed leaves the after empty.
func (a *fleetAdmin) changeSnapshots(ctx context.Context, attemptID string) (attempt.Record, string, string, error) {
	if a.attempts == nil || a.artifacts == nil {
		return attempt.Record{}, "", "", errors.New("attempts are not wired")
	}
	record, err := a.attempts.Get(ctx, attemptID)
	if err != nil {
		return attempt.Record{}, "", "", err
	}
	after := ""
	if record.Result != nil {
		after = record.Result.Artifact
	}
	return record, record.Base, after, nil
}

// Changes is what an attempt changed, as the reply keeps it.
func (a *fleetAdmin) Changes(ctx context.Context, attemptID string) (*readmodel.ChangeSummary, error) {
	record, base, after, err := a.changeSnapshots(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	summary := &readmodel.ChangeSummary{Attempt: attemptID, Project: record.Project, Base: base, Artifact: after}
	if after == "" || after == base {
		return summary, nil // nothing changed: the fold says so
	}
	changes, truncated, err := a.artifacts.Changes(ctx, record.Project, base, after)
	if err != nil {
		summary.Note = err.Error()
		return summary, nil
	}
	summary.Files = len(changes)
	if truncated {
		summary.Note = "只统计了前 " + fmt.Sprint(len(changes)) + " 个"
	}
	return summary, nil
}

// AttemptChanges is the files an attempt changed.
func (a *fleetAdmin) AttemptChanges(ctx context.Context, attemptID string) (readmodel.ChangeIndex, error) {
	record, base, after, err := a.changeSnapshots(ctx, attemptID)
	if err != nil {
		return readmodel.ChangeIndex{}, err
	}
	index := readmodel.ChangeIndex{Attempt: attemptID, Project: record.Project, Base: base, Artifact: after}
	if after == "" || after == base {
		index.Note = "没有改动"
		return index, nil
	}
	changes, truncated, err := a.artifacts.Changes(ctx, record.Project, base, after)
	if err != nil {
		return readmodel.ChangeIndex{}, err
	}
	index.Changes, index.Truncated = changes, truncated
	return index, nil
}

// AttemptDiff is one changed file's diff.
func (a *fleetAdmin) AttemptDiff(ctx context.Context, attemptID, path string) (readmodel.FileDiff, error) {
	record, base, after, err := a.changeSnapshots(ctx, attemptID)
	if err != nil {
		return readmodel.FileDiff{}, err
	}
	if after == "" {
		return readmodel.FileDiff{}, errors.New("没有改动")
	}
	diff, truncated, err := a.artifacts.FileDiff(ctx, record.Project, base, after, path)
	if err != nil {
		return readmodel.FileDiff{}, err
	}
	return readmodel.FileDiff{Path: path, Diff: diff, Truncated: truncated}, nil
}

// SetProjectMemory rewrites one project's memory whole, through the
// same lock the agents' writes take.
func (a *fleetAdmin) SetProjectMemory(ctx context.Context, id, text string) error {
	if a.memory == nil {
		return errors.New("memory is not wired")
	}
	if _, ok, err := a.projects.Get(ctx, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no project %q", id)
	}
	return a.memory.Replace(ctx, memory.ProjectScope(id), text, memory.Actor{By: "console"})
}

// SetHomeFile rewrites one of the three. The next turn reads it; a
// session already open is told its instructions changed.
func (a *fleetAdmin) SetHomeFile(ctx context.Context, name, text string) error {
	if a.homePath == "" {
		return errors.New("没有配置档案目录（gateway.home_path）")
	}
	if name == home.FileMemory && a.memory != nil {
		// The global memory is a scope: same lock, ids kept, audited.
		return a.memory.Replace(ctx, memory.Global, text, memory.Actor{By: "console"})
	}
	return home.Write(a.homePath, name, text)
}

func (a *fleetAdmin) RemoveProject(ctx context.Context, id string) error {
	if id == homeProjectID {
		return fmt.Errorf("%s 是 Steve 自己的家，不能移除", id)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	configMu.Lock()
	if id == a.cfg.Gateway.DefaultProject {
		configMu.Unlock()
		return fmt.Errorf("%s 是默认项目，先在配置里换一个默认项目再移除", id)
	}
	_, inConfig := a.cfg.Projects[id]
	if inConfig {
		saved := a.cfg.Projects[id]
		delete(a.cfg.Projects, id)
		if err := config.Save(a.path, a.cfg); err != nil {
			a.cfg.Projects[id] = saved
			configMu.Unlock()
			return fmt.Errorf("写 %s 失败：%w", a.path, err)
		}
	}
	configMu.Unlock()
	if _, ok, err := a.projects.Get(ctx, id); err != nil {
		return err
	} else if !ok && !inConfig {
		return fmt.Errorf("没有叫 %q 的项目", id)
	}
	if err := a.projects.Retire(ctx, id); err != nil {
		return err
	}
	if a.repos != nil {
		a.repos.wake()
	}
	log.Printf("steve: project %s retired", id)
	return nil
}

func (a *fleetAdmin) AddAgent(_ context.Context, req readmodel.AddAgentRequest) error {
	id := strings.ToLower(strings.TrimSpace(req.ID))
	if !nameShape.MatchString(id) {
		return fmt.Errorf("Agent 名只能是小写字母、数字、点、下划线、连字符")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.cfg.Harnesses[req.Harness]; !ok {
		return fmt.Errorf("hub 的 harnesses 里没有 %q；Agent 用的 AI 工具要先在 hub 配置", req.Harness)
	}
	if req.Node != "" {
		if _, ok := a.cfg.Nodes[req.Node]; !ok {
			return fmt.Errorf("没有叫 %q 的机器", req.Node)
		}
	}
	if _, exists := a.cfg.Agents[id]; exists {
		return fmt.Errorf("Agent %s 已经存在", id)
	}
	item := config.Agent{Harness: req.Harness, Node: req.Node, Model: req.Model}
	if err := a.catalog.Add(id, agent.Config{Harness: item.Harness, Node: item.Node, Model: item.Model}); err != nil {
		return err
	}
	if a.cfg.Agents == nil {
		a.cfg.Agents = map[string]config.Agent{}
	}
	a.cfg.Agents[id] = item
	if err := config.Save(a.path, a.cfg); err != nil {
		return fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	log.Printf("steve: agent %s added (%s on %s)", id, req.Harness, orHubName(req.Node))
	return nil
}

func orHubName(node string) string {
	if node == "" {
		return "hub"
	}
	return node
}

// Bootstrap is the script a new machine runs: it writes node.json with
// the hub's harness commands, fetches steve-node from the hub when the
// hub has one, and starts it from a login shell so the harnesses find
// their credentials.
func (a *fleetAdmin) Bootstrap(name, token string) (string, bool) {
	a.mu.Lock()
	n, ok := a.cfg.Nodes[name]
	harnesses := make(map[string]any, len(a.cfg.Harnesses))
	for id, h := range a.cfg.Harnesses {
		spec := map[string]any{"command": h.Command}
		if len(h.Args) > 0 {
			spec["args"] = h.Args
		}
		harnesses[id] = spec
	}
	binary, hubURL := a.cfg.Gateway.NodeBinary, a.hubURL
	a.mu.Unlock()
	if !ok || token == "" || subtle.ConstantTimeCompare([]byte(n.Token), []byte(token)) != 1 {
		return "", false
	}
	_, port, _ := net.SplitHostPort(n.Addr)
	if port == "" {
		port = "7701"
	}
	nodeJSON, _ := json.MarshalIndent(map[string]any{
		"name": name, "listen": "0.0.0.0:" + port, "token": token,
		"workspace_root": "~/steve-work", "state_dir": "~/.steve-node", "harnesses": harnesses,
	}, "", "  ")
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -e\nmkdir -p ~/steve-bin ~/steve-work\n")
	fmt.Fprintf(&b, "cat > ~/steve-bin/node.json <<'STEVE_EOF'\n%s\nSTEVE_EOF\nchmod 600 ~/steve-bin/node.json\n", nodeJSON)
	if binary != "" && hubURL != "" {
		fmt.Fprintf(&b, "if [ ! -x ~/steve-bin/steve-node ]; then curl -fsSL '%s/dist/steve-node?token=%s' -o ~/steve-bin/steve-node && chmod +x ~/steve-bin/steve-node; fi\n", hubURL, token)
	}
	b.WriteString("if [ ! -x ~/steve-bin/steve-node ]; then echo 'steve-node is not in ~/steve-bin; copy it there and run this again' >&2; exit 1; fi\n")
	b.WriteString("for p in $(pgrep -f 'steve-bin/steve-node -config' 2>/dev/null); do kill \"$p\" 2>/dev/null || true; done\n")
	b.WriteString("nohup bash -lc \"~/steve-bin/steve-node -config ~/steve-bin/node.json\" > ~/steve-node.log 2>&1 < /dev/null &\n")
	fmt.Fprintf(&b, "echo 'steve-node %s started; log: ~/steve-node.log'\n", name)
	return b.String(), true
}

func (a *fleetAdmin) NodeBinary(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.Gateway.NodeBinary == "" || token == "" {
		return "", false
	}
	for _, n := range a.cfg.Nodes {
		if subtle.ConstantTimeCompare([]byte(n.Token), []byte(token)) == 1 {
			return a.cfg.Gateway.NodeBinary, true
		}
	}
	return "", false
}

// repoCache keeps what each project's directory holds, asked of its
// machine every minute and whenever a project is added.
type repoCache struct {
	projects *project.Store
	nodes    *node.Registry
	hub      string

	mu    sync.Mutex
	repos map[string][]nodewire.Repo
	poke  chan struct{}
}

func (c *repoCache) get(id string) []nodewire.Repo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repos[id]
}

func (c *repoCache) wake() {
	select {
	case c.poke <- struct{}{}:
	default:
	}
}

func (c *repoCache) run(ctx context.Context) {
	for {
		c.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		case <-c.poke:
		}
	}
}

func (c *repoCache) pass(ctx context.Context) {
	list, err := c.projects.List(ctx)
	if err != nil {
		return
	}
	next := make(map[string][]nodewire.Repo, len(list))
	for _, p := range list {
		for _, ws := range p.Workspaces() {
			var repos []nodewire.Repo
			if ws.Node == "" || ws.Node == c.hub {
				repos = node.InspectRepos(ctx, ws.Path)
			} else {
				ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
				repos, err = c.nodes.Inspect(ictx, ws.Node, ws.Path)
				cancel()
				if err != nil {
					// Keep what we knew: a machine that is down did not
					// lose its repositories.
					repos = c.get(ws.ID)
				}
			}
			next[ws.ID] = repos
		}
	}
	c.mu.Lock()
	c.repos = next
	c.mu.Unlock()
}

// selectorsOf keeps every selector a session exposed, choices by label
// and by value, so the page can offer them and a pin can be matched.
func selectorsOf(options []steveview.Option) []models.Selector {
	var out []models.Selector
	for _, o := range options {
		sel := models.Selector{ID: o.ID, Name: o.Name, Category: o.Category, Current: o.Current}
		for _, c := range o.Choices {
			label := c.Label
			if label == "" {
				label = c.Value
			}
			sel.Choices = append(sel.Choices, label)
			sel.Values = append(sel.Values, c.Value)
		}
		out = append(out, sel)
	}
	return out
}

// hubSkills ships the enabled skills to every node. Nil until run wires it;
// doctor then reports the hub's skills as unknown rather than none.
var hubSkills *skillShipper

// skillShipper keeps every node's harness homes holding the same skills
// the hub enabled. The bundle is packed from the live map each time it is
// needed — skills are small, and a stale bundle would ship stale skills.
type skillShipper struct {
	nodes   *node.Registry
	live    *skills.Live
	observe func(kind, subject, text string)

	mu     sync.Mutex
	bundle skills.Bundle
	packed bool
}

func (s *skillShipper) pack() (skills.Bundle, error) {
	if s == nil || s.live == nil || s.live.Map == nil {
		return skills.Bundle{}, errors.New("skills are not configured")
	}
	refs, err := s.live.Map.Enabled()
	if err != nil {
		return skills.Bundle{}, err
	}
	b, err := skills.Pack(refs)
	if err != nil {
		return skills.Bundle{}, err
	}
	s.mu.Lock()
	s.bundle, s.packed = b, true
	s.mu.Unlock()
	return b, nil
}

// entries is what the hub's own machine has: the last packed bundle.
// hash is the bundle the hub last packed, or "" before the first pack.
func (s *skillShipper) hash() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.packed {
		return ""
	}
	return s.bundle.Hash
}

func (s *skillShipper) entries() ([]skills.Entry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bundle.Skills, s.packed
}

func (s *skillShipper) ship(ctx context.Context, name string) {
	b, err := s.pack()
	if err != nil {
		log.Printf("steve: skills for %s: %v", name, err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.nodes.PushSkills(ctx, name, b); err != nil {
		if strings.Contains(err.Error(), "does not take skill bundles") {
			return
		}
		log.Printf("steve: skills to %s: %v", name, err)
		if s.observe != nil {
			s.observe("node.skills", name, fmt.Sprintf("%s: skills %s not materialized: %v", name, b.Hash[:12], err))
		}
		return
	}
	if s.observe != nil {
		s.observe("node.skills", name, fmt.Sprintf("%s: skills %s materialized (%d skills)", name, b.Hash[:12], len(b.Skills)))
	}
}

func (s *skillShipper) shipAll(ctx context.Context) {
	if s == nil || s.nodes == nil {
		return
	}
	for _, st := range s.nodes.Statuses() {
		if st.Up {
			s.ship(ctx, st.Name)
		}
	}
}

// hubLaunch checks that the hub machine's own binaries start, the way a
// node checks its own. startHubLaunch runs it for the life of the process.
var hubLaunch = node.NewLaunchProbe()

func startHubLaunch(ctx context.Context, cfg *config.Config) {
	go hubLaunch.Run(ctx, func() []string {
		configMu.RLock()
		defer configMu.RUnlock()
		out := make([]string, 0, len(cfg.Harnesses)+len(cfg.Gateway.Tools))
		for _, h := range cfg.Harnesses {
			out = append(out, h.Command)
		}
		return append(out, cfg.Gateway.Tools...)
	})
}

// nodeName labels which machine ran a turn: the hub's own node name.
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

// capabilitiesFor adapts the capability assembler to what a step runner
// needs. A plan step opens its own session, so it needs the same identity and
// MCP servers an ordinary turn would get on that agent.
func capabilitiesFor(assembler *capability.Assembler) exec.Capabilities {
	return assembledCaps{assembler: assembler}
}

type assembledCaps struct{ assembler *capability.Assembler }

func (a assembledCaps) Assemble(candidate roster.Candidate) (string, []acp.MCPServer, error) {
	// Guest mode: a plan step is work, not a conversation with the owner, so
	// it gets the shared identity rather than the owner's private home.
	caps, err := a.assembler.AssembleMode(candidate.Agent, home.ModeGuest)
	if err != nil {
		return "", nil, err
	}
	return caps.Instructions + exec.ReportingContract, caps.MCPServers, nil
}

// taskBudget is the plan's brake. It charges each step to the task that owns
// the plan, so a plan cannot spend more than the work it belongs to was
// allowed — the same budget a chat turn is held to.
type taskBudget struct{ tasks *task.Store }

func (b taskBudget) Reserve(taskID string) (int, time.Time, error) {
	tracked, ok := b.tasks.Get(taskID)
	if !ok {
		return 0, time.Time{}, fmt.Errorf("task %s not found", taskID)
	}
	if limit, spent := tracked.Budget.Exhausted(); spent {
		return 0, time.Time{}, fmt.Errorf("task %s budget exhausted: %s", taskID, limit)
	}
	if _, err := b.tasks.Begin(taskID, tracked.Member, tracked.Node, ""); err != nil {
		return 0, time.Time{}, err
	}
	// Close the attempt straight away: a step's own success or failure is
	// recorded by the plan, and leaving the attempt open would make the task
	// look permanently mid-turn.
	if _, err := b.tasks.Finish(taskID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
		return 0, time.Time{}, err
	}
	left := tracked.Budget.MaxTurns - tracked.Budget.Turns - 1
	deadline := time.Now().Add(tracked.Budget.MaxElapsed - tracked.Budget.Elapsed)
	return max(0, left), deadline, nil
}

// choosePlanner is the one place the planning strategy is picked. A
// configured planning agent decomposes open goals with a model; without one
// the rule planner places declared steps and treats an open goal as a single
// step. Both produce the same validated Plan and the executor cannot tell
// which one did.
func choosePlanner(cfg *config.Config, catalog *agent.Catalog, manager *harness.Manager, workspaces project.Workspaces) planner.Planner {
	if cfg.Gateway.Planner == "" {
		return planner.Rule{}
	}
	selected, ok := catalog.Resolve(cfg.Gateway.Planner)
	if !ok {
		log.Printf("steve: planner agent %q not in catalog; using the rule planner", cfg.Gateway.Planner)
		return planner.Rule{}
	}
	log.Printf("steve: /plan decomposes with %s", selected.ID)
	return planner.LLM{
		Agent: selected.ID, Sessions: manager, Workspaces: workspaces,
		At: harness.Placement{Node: selected.Node, Harness: selected.Harness},
	}
}

// say sends one line to a running gateway's console and prints the reply:
// the page's send box, from a shell.
func say(args []string) error {
	flags := flag.NewFlagSet("say", flag.ContinueOnError)
	url := flags.String("url", defaultReadModelURL, "read model URL of a running gateway")
	token := flags.String("token", "", "token, when the read model is not on loopback")
	conversation := flags.String("conversation", "console:main", "console conversation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	input := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if input == "" {
		return errors.New("usage: steve say [-url …] [-token …] <text or /verb …>")
	}
	body, _ := json.Marshal(map[string]string{"conversation": *conversation, "input": input})
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *url+"/console/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("no gateway at %s — is `steve run` up? %w", *url, err)
	}
	defer res.Body.Close()
	var out struct {
		Error string `json:"error"`
		Reply struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"reply"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return fmt.Errorf("console at %s answered %s", *url, res.Status)
	}
	if out.Reply.Title != "" {
		fmt.Println("== " + out.Reply.Title)
	}
	fmt.Println(out.Reply.Text)
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return nil
}
