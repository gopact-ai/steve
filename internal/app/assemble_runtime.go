package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
)

func assembleRuntime(life lifetime, input inputAssembly) (runtimeAssembly, error) {
	configPath := input.ConfigPath()
	environment := input.Environment()
	parent := input.Parent()
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	life.Defer(func() { stop() })

	var configure func(*config.Config) error
	if environment != nil {
		configure = environment.Configure
	}
	cfg, catalog, manager, live, err := loadConfigured(*configPath, configure)
	if err != nil {
		return nil, err
	}
	life.Defer(func() { manager.Stop() })
	// One gateway per state directory, enforced before anything connects:
	// two processes on one Feishu app split the event stream between them.
	unlock, err := runtime.AcquireLock(filepath.Dir(cfg.Gateway.StatePath))
	if err != nil {
		return nil, err
	}
	life.Defer(func() { unlock() })
	var book *ledger.Ledger
	if environment != nil {
		book = environment.Ledger
		if book == nil {
			return nil, fmt.Errorf("coordinated application needs its generation ledger")
		}
		if environment.NodeID != "" {
			adminsvc.LocalNodeIdentity.Store(environment.NodeID)
		}
	} else {
		book, err = openLedger(cfg)
		if err != nil {
			return nil, err
		}
		life.Defer(func() { book.Close() })
	}
	// Cancel background owners before storage closes, including early exits
	// that happen before the execution registry is assembled.
	life.Defer(func() { stop() })
	if environment != nil && environment.SessionBinder != nil {
		manager.SetNodeSessionBinder(environment.SessionBinder)
		manager.SetStopRegistrar(execution.RegisterStopHandler)
	}
	background := newApplicationBackground(ctx)
	life.Defer(func() { background.Close() })
	return &runtimeValues{background: background, book: book, catalog: catalog, cfg: cfg, ctx: ctx, live: live, manager: manager, stop: stop}, nil
}

type runtimeAssembly interface {
	Background() *applicationBackground
	Book() *ledger.Ledger
	Catalog() *agent.Catalog
	Config() *config.Config
	Context() context.Context
	Live() *skills.Live
	Manager() *harness.Manager
	Stop() context.CancelFunc
}

type runtimeValues struct {
	background *applicationBackground
	book       *ledger.Ledger
	catalog    *agent.Catalog
	cfg        *config.Config
	ctx        context.Context
	live       *skills.Live
	manager    *harness.Manager
	stop       context.CancelFunc
}

func (v *runtimeValues) Background() *applicationBackground { return v.background }

func (v *runtimeValues) Book() *ledger.Ledger { return v.book }

func (v *runtimeValues) Catalog() *agent.Catalog { return v.catalog }

func (v *runtimeValues) Config() *config.Config { return v.cfg }

func (v *runtimeValues) Context() context.Context { return v.ctx }

func (v *runtimeValues) Live() *skills.Live { return v.live }

func (v *runtimeValues) Manager() *harness.Manager { return v.manager }

func (v *runtimeValues) Stop() context.CancelFunc { return v.stop }
