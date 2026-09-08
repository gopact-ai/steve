package admin

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

// Service adds machines and agents from the page: the running
// registry and catalog take them at once, and the config file records
// them so a restart keeps them. A new machine gets a token of its own
// and one command to run.
type Service struct {
	Observation        *LocalObservation
	ClusterMode        bool
	ssh                *sshconnect.Service
	releases           consoleapi.ReleaseProvider
	Owner              string
	MaterialLevel      project.Level
	Materials          *material.Store
	Console            *console.Service
	Lifetime           context.Context
	Mu                 sync.Mutex
	Cfg                *config.Config
	Path               string
	WriteConfig        func(string, *config.Config) error
	WriteConfigContext func(context.Context, string, *config.Config) error
	ConfigRevision     func() string
	cloning            map[string]bool
	cloneFiles         func(context.Context, string, nodewire.FileRequest) (string, error)
	cloneLeaseTTL      time.Duration
	cloneTimeout       time.Duration
	Nodes              *node.Registry
	Catalog            *agent.Catalog
	Fleet              *roster.Roster
	// projects is the ledger's project store; repos the hub's view of what
	// each project's directory holds.
	Projects *project.Store
	Repos    *RepoCache
	// attempts is where a copy's lock is taken before it is forgotten.
	Attempts *attempt.Service
	Tasks    *task.Store
	View     *readmodel.Model
	// artifacts is the project snapshots, for a turn's changes.
	Artifacts *artifact.Store
	// skills is the live map of what agents are handed, shipper what each
	// machine holds of it; coordinator holds the lock a change needs.
	LiveSkills  *skills.Live
	Shipper     *SkillShipper
	Coordinator *turn.Coordinator
	// homePath is Steve's own directory: who it is, who the owner is,
	// what it remembers.
	HomePath   string
	HomeLoader home.Loader
	SharedHome *memory.LedgerStore
	// memory is what Steve remembers, for the page to read and edit.
	Memory *memory.Service
	// probes remembers what each MCP deployment answered when last
	// probed, by "<machine>/<name>", with the shape it had then; a
	// deployment whose shape moved is shown as stale. provenance
	// remembers where an installed or adopted deployment came from.
	probeMu    sync.Mutex
	probes     map[string]mcpProbeEntry
	provenance map[string]string
	// manager and assembler take the hub machine's own harness and MCP
	// changes at runtime.
	Manager   *harness.Manager
	Assembler *capability.Assembler
	// hubURL is how the last page that added a machine reached the hub;
	// the bootstrap script fetches the binary from there.
	hubURL string
}

// ConfigMu guards the loaded configuration: the admin rewrites parts of
// it from the page while hubAdvert and the launch probe read it.
var ConfigMu sync.RWMutex

var NameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// nodeKey is the registry's name for a machine the page named: the hub
// is "" inside, whatever it is called outside.
func (a *Service) nodeKey(name string) string {
	if a.ClusterMode {
		if name == "" || name == "hub" {
			return NodeName()
		}
		return name
	}
	if name == "" || name == "hub" || name == nodewire.Place("") {
		return ""
	}
	return name
}

func orHubName(node string) string {
	if node == "" {
		return "hub"
	}
	return node
}

// persistConfig preserves the commit boundary for callers with derived live state.
func (a *Service) PersistConfig(candidate *config.Config) error {
	ctx := a.Lifetime
	if ctx == nil {
		ctx = context.Background()
	}
	return a.persistConfigContext(ctx, candidate)
}

func (a *Service) persistConfigContext(ctx context.Context, candidate *config.Config) error {
	write := a.WriteConfig
	if write == nil {
		write = config.Save
	}
	var err error
	if a.WriteConfigContext != nil {
		err = a.WriteConfigContext(ctx, a.Path, candidate)
	} else {
		err = write(a.Path, candidate)
	}
	if err != nil {
		if config.Committed(err) {
			a.Cfg.AdoptFileRevision(candidate)
			return fmt.Errorf("配置已应用，目录同步失败，持久性尚未确认：%w", err)
		}
		return fmt.Errorf("写 %s 失败：%w", a.Path, err)
	}
	a.Cfg.AdoptFileRevision(candidate)
	return nil
}
