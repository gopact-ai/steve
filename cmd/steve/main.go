package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gopact-ai/steve/internal/models"
	steveview "github.com/gopact-ai/steve/internal/view"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gopact-ai/acp"
	gopactsqlite "github.com/gopact-ai/gopact-ext/stores/sqlite"
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
		if p.Home.Node == node && p.ID != homeProjectID {
			return p.Home.Path, true
		}
	}
	for _, p := range projects {
		if p.Home.Node == node {
			return p.Home.Path, true
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
		seen.Observe(models.Observation{Node: at.Node, Harness: at.Harness, Current: s.Model, Available: s.Models, Version: s.Adapter, Source: "session"})
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
	view := readmodel.New(readmodel.Sources{
		Hub: readmodel.Hub{
			Node: nodeName(), Started: time.Now(), Capabilities: cfg.Gateway.Capabilities,
			Level: string(cfg.HubLevel()),
		},
		HubAdvert: func() nodewire.Advert { return hubAdvert(cfg) },
		Models:    seen,
		Roster:    fleet, Nodes: nodes, Tasks: tasks, Plans: plans,
		Ledger:       readmodel.Ledger{Book: book, Attempts: attempts, Artifacts: artifacts, Projects: projects, Intents: intents},
		Schedules:    schedules,
		Observations: book.Document("observations"),
	})
	if err := view.LoadObservations(); err != nil {
		log.Printf("steve: observations: %v", err)
	}
	// Machines coming and going are history, not just log lines.
	nodes.SetObserver(func(s node.Status) {
		if s.Up {
			view.Observe("node.up", s.Name, fmt.Sprintf("%s connected: %s %s/%s, build %s", s.Name, s.Advert.Hostname, s.Advert.OS, s.Advert.Arch, s.Advert.BuildVersion))
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
	dashboard.SetConsole(cons)
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
		delegation.MaxWait = time.Duration(cfg.Gateway.PromptTimeout) - 30*time.Second
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

// hubAdvert describes the hub's own machine the way a node's advert
// describes a node: the same harness check against this PATH, the same
// identity, so the fleet has one shape for every machine.
func hubAdvert(cfg *config.Config) nodewire.Advert {
	specs := make(map[string]node.HarnessSpec, len(cfg.Harnesses))
	for id, h := range cfg.Harnesses {
		specs[id] = node.HarnessSpec{Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir, Slots: h.Slots}
	}
	adv := node.Advertise(nodeName(), specs, cfg.Gateway.Capabilities)
	adv.StateDir = filepath.Dir(cfg.Gateway.StatePath)
	return adv
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
