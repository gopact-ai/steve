// Package app assembles and owns the lifetime of the hub application.
package app

import (
	"context"
	"sync"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
)

// Config selects the existing configuration file and optional cluster ports.
type Config struct {
	Path        string
	Environment *Environment
}

// App owns the assembled services and their ordered shutdown.
type App struct {
	run  func() error
	stop context.CancelFunc
	life *applicationLifetime
}

// Build assembles the subsystems in startup order. Background owners started
// during assembly retain ctx, and failed builds unwind every completed step.
func Build(ctx context.Context, cfg Config) (_ *App, buildErr error) {
	configureLogging()
	life := &applicationLifetime{}
	built := false
	defer func() {
		if !built {
			life.err = buildErr
			buildErr = life.Close()
		}
	}()
	life.Defer(func() {
		if services := life.services; services != nil && services.Requested() {
			if life.err == nil {
				life.err = &adminsvc.RestartExit{Service: services}
			} else {
				services.Failed(life.err)
			}
		}
	})
	input := &assemblyInput{parent: ctx, path: cfg.Path, environment: cfg.Environment}
	runtime, err := assembleRuntime(life, input)
	if err != nil {
		return nil, err
	}
	ledger, err := assembleLedger(runtime)
	if err != nil {
		return nil, err
	}
	home, err := assembleHome(input, runtime)
	if err != nil {
		return nil, err
	}
	fleet, err := assembleFleet(life, input, runtime)
	if err != nil {
		return nil, err
	}
	models, err := assembleModels(life, runtime, fleet)
	if err != nil {
		return nil, err
	}
	execution, err := assembleExecution(input, runtime, ledger, home, fleet)
	if err != nil {
		return nil, err
	}
	plans, err := assemblePlans(life, input, runtime, ledger, home, fleet, models, execution)
	if err != nil {
		return nil, err
	}
	readModel, err := assembleReadModel(runtime, ledger, fleet, models, execution, plans)
	if err != nil {
		return nil, err
	}
	console, err := assembleConsole(life, input, runtime, ledger, home, fleet, execution, plans, readModel)
	if err != nil {
		return nil, err
	}
	administration, err := assembleAdministration(life, input, runtime, execution, plans, readModel, console)
	if err != nil {
		return nil, err
	}
	if err := assembleFleetWorkers(runtime, ledger, fleet, models, execution, readModel); err != nil {
		return nil, err
	}
	delegation, err := assembleDelegation(input, runtime, ledger, home, fleet, execution, readModel, console)
	if err != nil {
		return nil, err
	}
	channels, err := assembleChannels(runtime, ledger, execution, readModel, console, administration, delegation)
	if err != nil {
		return nil, err
	}
	if err := assembleRecovery(input, runtime, ledger, execution, console); err != nil {
		return nil, err
	}
	built = true
	return &App{life: life, stop: runtime.Stop(), run: func() error {
		if err := startListeners(life, input, runtime, ledger, execution, console, administration, delegation, channels); err != nil {
			return err
		}
		return runChannel(runtime, ledger, home, execution, readModel, administration, channels)
	}}, nil
}

// Run waits for the application lifetime and joins shutdown before returning.
func (a *App) Run(ctx context.Context) (runErr error) {
	stop := context.AfterFunc(ctx, a.stop)
	defer stop()
	defer func() { a.life.err = runErr; runErr = a.Close() }()
	return a.run()
}

// Close releases a built application even when Run has not been entered.
func (a *App) Close() error { return a.life.Close() }

type lifetime interface {
	Defer(func())
	RunError() *error
	SetServices(*adminsvc.Services)
}

type applicationLifetime struct {
	once     sync.Once
	cleanup  []func()
	err      error
	services *adminsvc.Services
}

func (l *applicationLifetime) Defer(close func())                      { l.cleanup = append(l.cleanup, close) }
func (l *applicationLifetime) RunError() *error                        { return &l.err }
func (l *applicationLifetime) SetServices(services *adminsvc.Services) { l.services = services }
func (l *applicationLifetime) Close() error {
	l.once.Do(func() {
		for _, cleanup := range l.cleanup {
			defer cleanup()
		}
	})
	return l.err
}
