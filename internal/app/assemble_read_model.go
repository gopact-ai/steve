package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/readmodel"
	steveview "github.com/gopact-ai/steve/internal/view"
)

func assembleReadModel(input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, machines fleetAssembly, modelInfo modelsAssembly, work executionAssembly, planning plansAssembly) (readModelAssembly, error) {
	environment := input.Environment()
	background := boot.Background()
	book := boot.Book()
	cfg := boot.Config()
	ctx := boot.Context()
	live := boot.Live()
	manager := boot.Manager()
	attempts := storage.Attempts()
	fleet := machines.Fleet()
	nodes := machines.Nodes()
	observation := machines.Observation()
	projects := machines.Projects()
	seen := modelInfo.Seen()
	artifacts := work.Artifacts()
	intents := work.Intents()
	plans := work.Plans()
	schedules := work.Schedules()
	tasks := work.Tasks()
	stepRunner := planning.StepRunner()

	// One read model, two renderers. `steve top` and the browser are both
	// clients of this; neither reads the stores directly, so what the
	// operator sees in one place cannot contradict the other.
	// What each project's directory holds is asked of its machine on a
	// slow clock and kept: a page must not run git on every repaint.
	repos := &adminsvc.RepoCache{Projects: projects, Nodes: nodes, Hub: boot.NodeName(), Poke: make(chan struct{}, 1)}
	view := readmodel.New(readmodel.Sources{
		Hub: readmodel.Hub{
			Node: boot.NodeName(), Started: time.Now(), Capabilities: cfg.Gateway.Capabilities,
			Level: string(cfg.HubLevel()),
		},
		HubAdvert: func() nodewire.Advert {
			return adminsvc.ObservedHubAdvert(boot.NodeName(), boot.ConfigStore(), observation)
		},
		Repos: repos.Get, HomeProject: adminsvc.HomeProjectID, DefaultProject: cfg.Gateway.DefaultProject,
		Models: seen,
		Roster: fleet, Nodes: nodes, NodeNames: memberNames(environment), Tasks: tasks, Plans: plans,
		Ledger:       readmodel.Ledger{Book: book, Attempts: attempts, Artifacts: artifacts, Projects: projects, Intents: intents},
		Schedules:    schedules,
		Observations: readmodel.Observations{Book: book},
	})
	if err := view.LoadObservations(); err != nil {
		slog.Error(fmt.Sprintf("steve: observations: %v", err))
	}
	// A machine's abilities changing is history too: a tool that vanished
	// explains the placement that failed after it.
	// Keys: changes — one change per line, each "key\tfrom\tto"; an empty
	// from means the ability appeared, an empty to means it went away.
	nodes.SetDriftObserver(func(name string, changes []ability.Change) {
		said := make([]string, 0, len(changes))
		rows := make([]string, 0, len(changes))
		for _, c := range changes {
			said = append(said, c.String())
			rows = append(rows, c.Key+"\t"+c.From+"\t"+c.To)
		}
		view.Observe("node.manifest", name, name+": "+strings.Join(said, "; "), map[string]string{"changes": strings.Join(rows, "\n")})
	})
	// Skills are the hub's to enable and every machine's to have: each
	// node gets the enabled set as a content-addressed bundle when it
	// connects and whenever the set changes, before harnesses restart.
	shipper := &adminsvc.SkillShipper{Nodes: nodes, Live: live, Observe: view.Observe}
	observation.Skills.Store(shipper)
	if _, err := shipper.Pack(); err != nil {
		slog.Error(fmt.Sprintf("steve: skills could not be packed for nodes: %v", err))
	}
	live.After = func() error {
		remoteErr := shipper.ShipAll(ctx)
		// Links already changed under skills admission. Retire old hosts
		// even when propagation is pending; neither error acknowledges apply.
		localErr := manager.Restart()
		return errors.Join(remoteErr, localErr)
	}
	// Machines coming and going are history, not just log lines.
	nodes.SetObserver(nodeObserver(view.Observe, func(s node.Status) {
		background.Go(func(ctx context.Context) { shipper.Ship(ctx, s.Name) })
		// A machine that comes back may hold worktrees of attempts that
		// died with the connection; nothing else ever returns for them.
		if root := s.Advert.WorkspaceRoot; root != "" {
			background.Go(func(ctx context.Context) {
				sweepWorktrees(ctx, artifacts, attempts, tasks, view, boot.NodeName(), s.Name, root)
			})
		}
	}))
	stepRunner.SetObserver(func(req exec.StepRequest, p steveview.Progress, ended bool) {
		if ended {
			view.StepEnded(req.TaskID, req.PlanID, req.StepID, req.Agent, req.Node, p)
			return
		}
		view.StepProgress(req.TaskID, req.PlanID, req.StepID, req.Agent, req.Node, p)
	})
	return &readModelValues{repos: repos, shipper: shipper, view: view}, nil
}

// nodeObserver records machines coming and going as history, and hands a
// machine's arrival to arrived: what a connection sets going on the machine.
func nodeObserver(record func(kind, subject, text string, data map[string]string), arrived func(node.Status)) func(node.Status) {
	return func(s node.Status) {
		if s.Up {
			// Keys: host, os, arch, build.
			record("node.up", s.Name, fmt.Sprintf("%s connected: %s %s/%s, build %s", s.Name, s.Advert.Hostname, s.Advert.OS, s.Advert.Arch, s.Advert.BuildVersion),
				map[string]string{"host": s.Advert.Hostname, "os": s.Advert.OS, "arch": s.Advert.Arch, "build": s.Advert.BuildVersion})
			arrived(s)
			return
		}
		// Keys: reason.
		record("node.down", s.Name, fmt.Sprintf("%s disconnected: %s", s.Name, s.LastError), map[string]string{"reason": s.LastError})
	}
}

type readModelAssembly interface {
	Repos() *adminsvc.RepoCache
	Shipper() *adminsvc.SkillShipper
	View() *readmodel.Model
}

type readModelValues struct {
	repos   *adminsvc.RepoCache
	shipper *adminsvc.SkillShipper
	view    *readmodel.Model
}

func (v *readModelValues) Repos() *adminsvc.RepoCache { return v.repos }

func (v *readModelValues) Shipper() *adminsvc.SkillShipper { return v.shipper }

func (v *readModelValues) View() *readmodel.Model { return v.view }

// memberNames exposes cluster display names to the read model; a hub
// outside a cluster has none.
func memberNames(environment *Environment) func() map[string]string {
	if environment == nil || environment.Coordination == nil {
		return nil
	}
	return environment.Coordination.MemberNames
}
