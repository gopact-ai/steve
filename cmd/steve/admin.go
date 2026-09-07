package main

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/sshconnect"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// fleetAdmin adds machines and agents from the page: the running
// registry and catalog take them at once, and the config file records
// them so a restart keeps them. A new machine gets a token of its own
// and one command to run.
type fleetAdmin struct {
	observation        *localObservation
	clusterMode        bool
	ssh                *sshconnect.Service
	releases           consoleapi.ReleaseProvider
	owner              string
	materialLevel      project.Level
	materials          *material.Store
	console            *console.Service
	lifetime           context.Context
	mu                 sync.Mutex
	cfg                *config.Config
	path               string
	writeConfig        func(string, *config.Config) error
	writeConfigContext func(context.Context, string, *config.Config) error
	configRevision     func() string
	cloning            map[string]bool
	cloneFiles         func(context.Context, string, nodewire.FileRequest) (string, error)
	cloneLeaseTTL      time.Duration
	cloneTimeout       time.Duration
	nodes              *node.Registry
	catalog            *agent.Catalog
	fleet              *roster.Roster
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
	homePath   string
	homeLoader home.Loader
	sharedHome *memory.LedgerStore
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

var nameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// nodeKey is the registry's name for a machine the page named: the hub
// is "" inside, whatever it is called outside.
func (a *fleetAdmin) nodeKey(name string) string {
	if a.clusterMode {
		if name == "" || name == "hub" {
			return nodeName()
		}
		return name
	}
	if name == "" || name == "hub" || name == nodewire.Place("") {
		return ""
	}
	return name
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func orHubName(node string) string {
	if node == "" {
		return "hub"
	}
	return node
}

// persistConfig preserves the commit boundary for callers with derived live state.
func (a *fleetAdmin) persistConfig(candidate *config.Config) error {
	ctx := a.lifetime
	if ctx == nil {
		ctx = context.Background()
	}
	return a.persistConfigContext(ctx, candidate)
}

func (a *fleetAdmin) persistConfigContext(ctx context.Context, candidate *config.Config) error {
	write := a.writeConfig
	if write == nil {
		write = config.Save
	}
	var err error
	if a.writeConfigContext != nil {
		err = a.writeConfigContext(ctx, a.path, candidate)
	} else {
		err = write(a.path, candidate)
	}
	if err != nil {
		if config.Committed(err) {
			a.cfg.AdoptFileRevision(candidate)
			return fmt.Errorf("配置已应用，目录同步失败，持久性尚未确认：%w", err)
		}
		return fmt.Errorf("写 %s 失败：%w", a.path, err)
	}
	a.cfg.AdoptFileRevision(candidate)
	return nil
}
