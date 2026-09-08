package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/readmodel"
	steveview "github.com/gopact-ai/steve/internal/view"
)

func assembleReadModel(boot runtimeAssembly, storage ledgerAssembly, machines fleetAssembly, modelInfo modelsAssembly, work executionAssembly, planning plansAssembly) (readModelAssembly, error) {
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
	repos := &adminsvc.RepoCache{Projects: projects, Nodes: nodes, Hub: adminsvc.NodeName(), Poke: make(chan struct{}, 1)}
	view := readmodel.New(readmodel.Sources{
		Hub: readmodel.Hub{
			Node: adminsvc.NodeName(), Started: time.Now(), Capabilities: cfg.Gateway.Capabilities,
			Level: string(cfg.HubLevel()),
		},
		HubAdvert: func() nodewire.Advert { return adminsvc.ObservedHubAdvert(cfg, observation) },
		Repos:     repos.Get, HomeProject: adminsvc.HomeProjectID, DefaultProject: cfg.Gateway.DefaultProject,
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
	shipper := &adminsvc.SkillShipper{Nodes: nodes, Live: live, Observe: view.Observe}
	observation.Skills.Store(shipper)
	if _, err := shipper.Pack(); err != nil {
		log.Printf("steve: skills could not be packed for nodes: %v", err)
	}
	live.After = func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		shipper.ShipAll(ctx)
		return manager.Restart()
	}
	// Machines coming and going are history, not just log lines.
	nodes.SetObserver(func(s node.Status) {
		if s.Up {
			view.Observe("node.up", s.Name, fmt.Sprintf("%s connected: %s %s/%s, build %s", s.Name, s.Advert.Hostname, s.Advert.OS, s.Advert.Arch, s.Advert.BuildVersion))
			background.Go(func(ctx context.Context) { shipper.Ship(ctx, s.Name) })
			// A machine that comes back may hold worktrees of attempts that
			// died with the connection; nothing else ever returns for them.
			if root := s.Advert.WorkspaceRoot; root != "" {
				background.Go(func(ctx context.Context) { sweepWorktrees(ctx, artifacts, attempts, tasks, view, s.Name, root) })
			}
			return
		}
		view.Observe("node.down", s.Name, fmt.Sprintf("%s disconnected: %s", s.Name, s.LastError))
	})
	stepRunner.SetObserver(func(req exec.StepRequest, p steveview.Progress) {
		view.StepProgress(req.TaskID, req.PlanID, req.StepID, req.Agent, req.Node, p)
	})
	return &readModelValues{repos: repos, shipper: shipper, view: view}, nil
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
