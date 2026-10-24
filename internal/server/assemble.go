// Package server is Steve's composition root: it opens the kernel's stores
// and execution machinery, puts the product services on top, and serves them
// over the /v2 HTTP API.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/approvals"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/chief"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/feed"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/thread"
	"github.com/gopact-ai/steve/internal/work"
)

// Server is one running Steve.
type Server struct {
	Config    *config.Config
	Feed      *feed.Feed
	Work      *work.Service
	Approvals *approvals.Service
	Thread    *thread.Service
	Chief     *chief.Chief
	Token     string

	book     *ledger.Ledger
	manager  *harness.Manager
	nodes    *node.Registry
	closers  []func()
	cancel   context.CancelFunc
	ctx      context.Context
	nodeName string
}

// Options adjust what New assembles; the zero value is production.
type Options struct {
	// ApprovalTimeout bounds how long a card waits for the person.
	ApprovalTimeout time.Duration
}

// New assembles a server from a loaded configuration. Close releases it.
func New(parent context.Context, cfg *config.Config, opts Options) (s *Server, err error) {
	ctx, cancel := context.WithCancel(parent)
	s = &Server{Config: cfg, cancel: cancel, ctx: ctx, nodeName: nodeName()}
	defer func() {
		if err != nil {
			s.Close()
			s = nil
		}
	}()
	if err := cfg.ValidateChannels(); err != nil {
		return s, err
	}
	skillMap, err := s.openRuntime()
	if err != nil {
		return s, err
	}
	k, err := s.openKernel()
	if err != nil {
		return s, err
	}
	err = s.assembleProduct(k, skillMap, opts)
	return s, err
}

// kernel is what the product services are built on.
type kernel struct {
	catalog    *agent.Catalog
	fleet      *roster.Roster
	attempts   *attempt.Service
	artifacts  *artifact.Store
	tasks      *task.Store
	executions *execution.Registry
}

// openRuntime takes the state directory and prepares the harnesses: each
// gets an isolated home with the configured skills installed.
func (s *Server) openRuntime() (*skills.Map, error) {
	cfg := s.Config
	stateDir := filepath.Dir(cfg.Gateway.StatePath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := runtime.AcquireLock(stateDir)
	if err != nil {
		return nil, err
	}
	s.defer_(unlock)
	harnesses := make([]string, 0, len(cfg.Harnesses))
	for name := range cfg.Harnesses {
		harnesses = append(harnesses, name)
	}
	if err := runtime.PrepareSelected(stateDir, harnesses); err != nil {
		return nil, err
	}
	skillMap, err := skills.Setup(stateDir)
	if err != nil {
		return nil, err
	}
	live := &skills.Live{Map: skillMap, Dests: runtime.SelectedSkillDests(stateDir, harnesses)}
	if err := live.Apply(); err != nil {
		return nil, err
	}
	s.manager, err = configbuild.HarnessManager(cfg)
	if err != nil {
		return nil, err
	}
	s.defer_(s.manager.Stop)
	return skillMap, nil
}

// openKernel opens the ledger and everything that keeps executions correct:
// machines, projects, the roster, attempts (classifying what a previous
// process left), artifacts, tasks and the execution registry.
func (s *Server) openKernel() (k kernel, err error) {
	cfg, ctx := s.Config, s.ctx
	stateDir := filepath.Dir(cfg.Gateway.StatePath)
	hubID, err := resolveHubID(stateDir)
	if err != nil {
		return k, err
	}
	if k.catalog, err = cfg.AgentCatalog(); err != nil {
		return k, err
	}
	s.book, err = ledger.Open(stateDir, ledger.Options{})
	if errors.Is(err, ledger.ErrRecoveryRequired) {
		return k, fmt.Errorf("%w\nrun: steve ledger recover --config <config>", err)
	}
	if err != nil {
		return k, err
	}
	s.defer_(func() { s.book.Close() })
	nodewire.SetSelf(s.nodeName)
	s.nodes = node.NewRegistry(hubID, configbuild.NodeConfigs(cfg))
	s.defer_(func() { s.nodes.Close() })
	s.manager.SetTransports(s.nodes)
	projects := project.Open(s.book, artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
	projects.SetHubID(hubID)
	localized := i18n.WithLocale(ctx, i18n.FromLang(cfg.EffectiveLocale()))
	if err := (configbuild.ProjectController{Store: projects}).Reconcile(localized, cfg); err != nil {
		return k, fmt.Errorf("reconcile configured projects: %w", err)
	}
	k.fleet = roster.New(k.catalog)
	k.fleet.SetNodes(s.nodes)
	k.fleet.SetHubCapabilities(cfg.Gateway.Capabilities)
	k.fleet.SetHubAdvert(func() nodewire.Advert { return hubAdvert(s.nodeName, cfg) })
	k.fleet.SetHubLevel(cfg.HubLevel())
	k.attempts = attempt.New(s.book)
	recovery, err := k.attempts.PrepareRecovery(ctx, "startup")
	if err != nil {
		return k, fmt.Errorf("prepare previous executions for recovery: %w", err)
	}
	for _, r := range recovery.Quarantined {
		slog.Warn("steve: quarantined previous writer", "attempt", r.ID, "task", r.TaskID, "node", r.Node)
	}
	k.artifacts = openArtifacts(ctx, stateDir, s.book, projects, s.nodes, cfg.Policies)
	if k.tasks, err = task.OpenLedger(s.book); err != nil {
		return k, fmt.Errorf("open tasks: %w", err)
	}
	k.tasks.SetBudget(cfg.Gateway.TaskMaxTurns, time.Duration(cfg.Gateway.TaskMaxElapsed))
	k.executions = execution.New(ctx, k.tasks)
	k.artifacts.SetExecution(k.executions)
	return k, nil
}

func openArtifacts(ctx context.Context, stateDir string, book *ledger.Ledger, projects *project.Store, nodes *node.Registry, p config.Policies) *artifact.Store {
	artifacts := artifact.New(filepath.Join(stateDir, "artifacts"), book, projects, nodes)
	artifacts.Limits = gitrepo.Limits{MaxFiles: p.Snapshot.MaxFiles, MaxBytes: p.Snapshot.MaxBytes, MaxFileBytes: p.Snapshot.MaxFileBytes}
	artifacts.Review = gitrepo.ReviewLimits{MaxChanges: p.Review.MaxChanges, MaxDiffBytes: p.Review.MaxDiffBytes, MaxFileBytes: p.Review.MaxFileBytes, MaxEntries: p.Review.MaxEntries, Timeout: time.Duration(p.Review.Timeout)}
	if _, err := artifacts.RecoverLandings(ctx); err != nil {
		slog.Error("steve: recover landings", "error", err)
	}
	return artifacts
}

// assembleProduct puts the feed, the task service, approvals, the thread and
// the chief of staff on the kernel.
func (s *Server) assembleProduct(k kernel, skillMap *skills.Map, opts Options) error {
	cfg, ctx := s.Config, s.ctx
	epoch, err := randomID()
	if err != nil {
		return err
	}
	s.Feed = feed.New(epoch, 4096)
	runner := agentexec.New(s.manager, k.fleet, k.artifacts, k.attempts, k.executions, work.Budget{Tasks: k.tasks})
	runner.SetCapabilities(guestCapabilities{configbuild.CapabilityAssembler(cfg).SetSkills(skillMap)})
	s.Work = work.New(ctx, s.book, k.tasks, runner, k.executions, s.Feed, work.Options{Agent: k.catalog.Default().ID, Project: cfg.Gateway.DefaultProject, Mode: work.ModeAuto})
	s.Approvals = approvals.New(approvalModes{s.Work, k.tasks}, s.Feed, opts.ApprovalTimeout)
	runner.SetQuestionHandlers(s.Approvals.Ask, s.Approvals.AskUser)
	runner.SetObserver(s.Work.Observe)
	s.Thread = thread.New(s.book, s.Feed)
	s.Chief = chief.New(ctx, s.Thread, s.Work, k.tasks, runner, chief.Options{Agent: chiefAgent(cfg, k.catalog.Default().ID), Project: cfg.Gateway.DefaultProject, Locale: cfg.EffectiveLocale()})
	s.Token = config.ConsoleToken(cfg.Gateway.ReadModelToken)
	return nil
}

// Close stops what is running and releases the stores, in reverse order.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.cancel()
	if s.Chief != nil {
		s.Chief.Wait()
	}
	if s.Work != nil {
		s.Work.Wait()
	}
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	s.closers = nil
}

func (s *Server) defer_(f func()) { s.closers = append(s.closers, f) }

// chiefAgent is the agent the chief of staff runs as: STEVE_CHIEF_AGENT
// when set, otherwise the default agent.
func chiefAgent(cfg *config.Config, fallback string) string {
	if id := strings.TrimSpace(os.Getenv("STEVE_CHIEF_AGENT")); id != "" {
		if _, ok := cfg.Agents[id]; ok {
			return id
		}
	}
	return fallback
}

// approvalModes answers for the task service, except that the chief's own
// turns are refused whatever they ask.
type approvalModes struct {
	*work.Service
	tasks *task.Store
}

func (m approvalModes) ModeOf(id string) string {
	if t, ok := m.tasks.Get(id); ok && chief.IsTurn(t) {
		return approvals.ModeDeny
	}
	return m.Service.ModeOf(id)
}

// guestCapabilities gives executors the shared guest identity and the MCP
// servers their agent is configured with; the owner's private home is not
// theirs.
type guestCapabilities struct{ assembler *capability.Assembler }

func (g guestCapabilities) Assemble(c roster.Candidate) (string, []acp.MCPServer, error) {
	caps, err := g.assembler.AssembleMode(c.Agent, home.ModeGuest)
	if err != nil {
		return "", nil, err
	}
	return caps.Instructions, caps.MCPServers, nil
}

// hubAdvert is what this machine offers: its harnesses and capabilities.
func hubAdvert(name string, cfg *config.Config) nodewire.Advert {
	specs := make(map[string]node.HarnessSpec, len(cfg.Harnesses))
	for id, h := range cfg.Harnesses {
		specs[id] = node.HarnessSpec{Command: h.Command, Args: h.Args, Env: h.Env, ProcessDir: h.ProcessDir, Slots: h.Slots}
	}
	return node.Advertise(name, specs, cfg.Gateway.Capabilities)
}

// resolveHubID is this installation's stable identity, created once.
func resolveHubID(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "hub-id")
	raw, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	id = "hub-" + id
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func nodeName() string {
	if name := strings.TrimSpace(os.Getenv("STEVE_NODE")); name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "local"
}
