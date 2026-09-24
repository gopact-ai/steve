// Package app assembles and owns the lifetime of the hub application.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/logs"
	"github.com/gopact-ai/steve/internal/turn"

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
	logs.Install()
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
				life.err = &adminsvc.RestartExit{Service: services, Program: services.PendingProgram()}
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
	readModel, err := assembleReadModel(input, runtime, ledger, fleet, models, execution, plans)
	if err != nil {
		return nil, err
	}
	console, err := assembleConsole(life, input, runtime, ledger, home, fleet, execution, plans, readModel)
	if err != nil {
		return nil, err
	}
	if err := assemblePlugins(life, runtime, fleet, console, input); err != nil {
		return nil, err
	}
	administration, err := assembleAdministration(life, input, runtime, execution, plans, readModel, console)
	if err != nil {
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
	wireCoordinator(execution, plans, administration, delegation, channels)
	// The workers, the messaging server and recovery below can reach the
	// coordinator, so they start only after it is wired.
	if err := assembleFleetWorkers(runtime, ledger, fleet, models, execution, readModel); err != nil {
		return nil, err
	}
	startMessaging(runtime, delegation)
	if err := assembleRecovery(input, runtime, ledger, execution, console); err != nil {
		return nil, err
	}
	assembleNodeReceipts(input, runtime, ledger, fleet)
	built = true
	return &App{life: life, stop: runtime.Stop(), run: func() error {
		if err := startListeners(life, input, runtime, ledger, execution, console, administration, delegation, channels); err != nil {
			return err
		}
		return runChannel(runtime, ledger, home, execution, readModel, administration, channels)
	}}, nil
}

// wireCoordinator hands the coordinator the callbacks the stages after
// execution built around it.
func wireCoordinator(work executionAssembly, planning plansAssembly, management administrationAssembly, delegates delegationAssembly, channels channelsAssembly) {
	work.Coordinator().Wire(coordinatorCallbacks(planning.Supervisor(), management.WorkspaceAttach(), delegates.Messaging(), channels.Routes()))
}

// coordinatorCallbacks assembles turn.Callbacks from what each stage built.
func coordinatorCallbacks(supervisor turn.Supervisor, attach func(ctx context.Context, projectID, node string) error, messaging messagingCallbacks, routes taskRoutes) turn.Callbacks {
	return turn.Callbacks{
		Supervisor:       supervisor,
		WorkspaceAttach:  attach,
		AgentGate:        messaging.AgentGate,
		AfterTurn:        messaging.AfterTurn,
		TurnPreface:      messaging.TurnPreface,
		Notifier:         routes.Notifier,
		Resumer:          routes.Resumer,
		ResumeDispatcher: routes.ResumeDispatcher,
	}
}

// Run waits for the application lifetime and joins shutdown before returning.
func (a *App) Run(ctx context.Context) (runErr error) {
	var stopping time.Time
	var stopMu sync.Mutex
	stop := context.AfterFunc(ctx, func() {
		stopMu.Lock()
		stopping = time.Now()
		stopMu.Unlock()
		a.stop()
	})
	defer stop()
	defer func() {
		stopMu.Lock()
		since := stopping
		stopMu.Unlock()
		if !since.IsZero() {
			slog.Info("app: application run returned after stop", "took", time.Since(since).Round(time.Millisecond))
		}
		a.life.err = runErr
		started := time.Now()
		runErr = a.Close()
		slog.Info("app: application closed", "took", time.Since(started).Round(time.Millisecond))
	}()
	return a.run()
}

// Close releases a built application even when Run has not been entered.
func (a *App) Close() error { return a.life.Close() }

type lifetime interface {
	AfterClose(func() error)
	Defer(func())
	RunError() *error
	SetServices(*adminsvc.Services)
}

type applicationLifetime struct {
	afterClose []func() error
	once       sync.Once
	cleanup    []shutdownStep
	err        error
	services   *adminsvc.Services
	// slowStep is how long one shutdown step may take before it is
	// reported; zero means defaultSlowStep.
	slowStep time.Duration
}

const defaultSlowStep = time.Second

// shutdownStep is one cleanup together with where it was registered, which
// names the component when the step is slow.
type shutdownStep struct {
	at  string
	run func()
}

func (l *applicationLifetime) Defer(close func()) {
	at := "unknown"
	if _, file, line, ok := runtime.Caller(1); ok {
		at = fmt.Sprintf("%s:%d", filepath.Base(file), line)
	}
	l.cleanup = append(l.cleanup, shutdownStep{at: at, run: close})
}
func (l *applicationLifetime) RunError() *error                        { return &l.err }
func (l *applicationLifetime) SetServices(services *adminsvc.Services) { l.services = services }
func (l *applicationLifetime) AfterClose(f func() error)               { l.afterClose = append(l.afterClose, f) }

func (l *applicationLifetime) Close() error {
	l.once.Do(func() {
		defer func() {
			for _, close := range l.afterClose {
				l.err = errors.Join(l.err, close())
			}
		}()
		slow := l.slowStep
		if slow <= 0 {
			slow = defaultSlowStep
		}
		for _, step := range l.cleanup {
			defer func() {
				started := time.Now()
				step.run()
				if took := time.Since(started); took >= slow {
					slog.Warn("app: shutdown step is slow", "step", step.at, "took", took.Round(time.Millisecond))
				}
			}()
		}
	})
	return l.err
}
