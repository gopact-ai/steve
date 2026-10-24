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
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/approvals"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/artifact/gitrepo"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
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
	"github.com/gopact-ai/steve/internal/work"
)

// Server is one running Steve.
type Server struct {
	Config    *config.Config
	Feed      *feed.Feed
	Work      *work.Service
	Approvals *approvals.Service
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
	stateDir := filepath.Dir(cfg.Gateway.StatePath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return s, err
	}
	unlock, err := runtime.AcquireLock(stateDir)
	if err != nil {
		return s, err
	}
	s.defer_(unlock)
	if err := cfg.ValidateChannels(); err != nil {
		return s, err
	}
	hubID, err := resolveHubID(stateDir)
	if err != nil {
		return s, err
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		return s, err
	}

	// Runtimes: each harness gets an isolated home, and the configured
	// skills are installed into it.
	harnesses := make([]string, 0, len(cfg.Harnesses))
	for name := range cfg.Harnesses {
		harnesses = append(harnesses, name)
	}
	if err := runtime.PrepareSelected(stateDir, harnesses); err != nil {
		return s, err
	}
	skillMap, err := skills.Setup(stateDir)
	if err != nil {
		return s, err
	}
	live := &skills.Live{Map: skillMap, Dests: runtime.SelectedSkillDests(stateDir, harnesses)}
	if err := live.Apply(); err != nil {
		return s, err
	}
	s.manager, err = configbuild.HarnessManager(cfg)
	if err != nil {
		return s, err
	}
	s.defer_(s.manager.Stop)

	s.book, err = ledger.Open(stateDir, ledger.Options{})
	if errors.Is(err, ledger.ErrRecoveryRequired) {
		return s, fmt.Errorf("%w\nrun: steve ledger recover --config <config>", err)
	}
	if err != nil {
		return s, err
	}
	s.defer_(func() { s.book.Close() })

	// Machines and projects.
	nodewire.SetSelf(s.nodeName)
	s.nodes = node.NewRegistry(hubID, configbuild.NodeConfigs(cfg))
	s.defer_(func() { s.nodes.Close() })
	s.manager.SetTransports(s.nodes)
	projects := project.Open(s.book, artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
	projects.SetHubID(hubID)
	localized := i18n.WithLocale(ctx, i18n.FromLang(cfg.EffectiveLocale()))
	if err := (configbuild.ProjectController{Store: projects}).Reconcile(localized, cfg); err != nil {
		return s, fmt.Errorf("reconcile configured projects: %w", err)
	}
	fleet := roster.New(catalog)
	fleet.SetNodes(s.nodes)
	fleet.SetHubCapabilities(cfg.Gateway.Capabilities)
	fleet.SetHubAdvert(func() nodewire.Advert { return hubAdvert(s.nodeName, cfg) })
	fleet.SetHubLevel(cfg.HubLevel())

	// Executions: attempts left by a previous process are classified
	// before anything new runs.
	attempts := attempt.New(s.book)
	recovery, err := attempts.PrepareRecovery(ctx, "startup")
	if err != nil {
		return s, fmt.Errorf("prepare previous executions for recovery: %w", err)
	}
	for _, r := range recovery.Quarantined {
		slog.Warn("steve: quarantined previous writer", "attempt", r.ID, "task", r.TaskID, "node", r.Node)
	}
	artifacts := artifact.New(filepath.Join(stateDir, "artifacts"), s.book, projects, s.nodes)
	p := cfg.Policies
	artifacts.Limits = gitrepo.Limits{MaxFiles: p.Snapshot.MaxFiles, MaxBytes: p.Snapshot.MaxBytes, MaxFileBytes: p.Snapshot.MaxFileBytes}
	artifacts.Review = gitrepo.ReviewLimits{MaxChanges: p.Review.MaxChanges, MaxDiffBytes: p.Review.MaxDiffBytes, MaxFileBytes: p.Review.MaxFileBytes, MaxEntries: p.Review.MaxEntries, Timeout: time.Duration(p.Review.Timeout)}
	if _, err := artifacts.RecoverLandings(ctx); err != nil {
		slog.Error("steve: recover landings", "error", err)
	}
	tasks, err := task.OpenLedger(s.book)
	if err != nil {
		return s, fmt.Errorf("open tasks: %w", err)
	}
	tasks.SetBudget(cfg.Gateway.TaskMaxTurns, time.Duration(cfg.Gateway.TaskMaxElapsed))
	executions := execution.New(ctx, tasks)
	artifacts.SetExecution(executions)

	// Product.
	epoch, err := randomID()
	if err != nil {
		return s, err
	}
	s.Feed = feed.New(epoch, 4096)
	runner := agentexec.New(s.manager, fleet, artifacts, attempts, executions, work.Budget{Tasks: tasks})
	runner.SetCapabilities(guestCapabilities{configbuild.CapabilityAssembler(cfg).SetSkills(skillMap)})
	s.Work = work.New(ctx, s.book, tasks, runner, executions, s.Feed, work.Options{Agent: catalog.Default().ID, Project: cfg.Gateway.DefaultProject, Mode: work.ModeAuto})
	s.Approvals = approvals.New(s.Work, s.Feed, opts.ApprovalTimeout)
	runner.SetQuestionHandlers(s.Approvals.Ask, s.Approvals.AskUser)
	runner.SetObserver(s.Work.Observe)

	s.Token = config.ConsoleToken(cfg.Gateway.ReadModelToken)
	return s, nil
}

// Close stops what is running and releases the stores, in reverse order.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.cancel()
	if s.Work != nil {
		s.Work.Wait()
	}
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	s.closers = nil
}

func (s *Server) defer_(f func()) { s.closers = append(s.closers, f) }

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
