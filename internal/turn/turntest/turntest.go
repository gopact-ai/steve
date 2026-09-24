// Package turntest builds turn coordinators for tests in other packages,
// so a test names only what it cares about. Everything it does not set is
// a real store or service on a temporary ledger, except the agent runtime,
// model prober and supervisor, which default to the stand-ins NoRuntime,
// NoProber and IdleSupervisor. IdleCoordinator stands in for a coordinator
// itself, in tests of code that is given one.
package turntest

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// Options is what a test sets before the defaults fill the rest.
type Options struct {
	turn.Deps
	// Ledger backs every default store; nil opens one in a temporary
	// directory. Stores a test sets itself should share it, since the
	// coordinator writes tasks, attempts and projects in one transaction.
	Ledger *ledger.Ledger
	// Callbacks are what New wires, with Callbacks filling what is unset.
	// Unwired ignores them.
	Callbacks turn.Callbacks
}

// Option sets part of Options.
type Option func(*Options)

// Deps is turn.Deps with every dependency the options leave unset filled:
// an empty agent catalog, a runtime that starts no agent, a prober that
// finds nothing, an empty home and skill set, the Chinese catalog, and every
// store on the options' ledger.
//
// fillDeps in the turn package's own tests fills Deps the same way, since
// those tests cannot import this package; change both together.
func Deps(t testing.TB, opts ...Option) turn.Deps {
	t.Helper()
	return fill(t, options(opts))
}

func options(opts []Option) Options {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func fill(t testing.TB, o Options) turn.Deps {
	t.Helper()
	if o.Ledger == nil {
		book, err := ledger.Open(t.TempDir(), ledger.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = book.Close() })
		o.Ledger = book
	}
	d, book := &o.Deps, o.Ledger
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if d.Catalog == nil {
		d.Catalog, err = agent.NewCatalog(nil)
		must(err)
	}
	if d.Store == nil {
		d.Store, err = state.OpenLedger(book)
		must(err)
	}
	if d.Assembler == nil {
		d.Assembler = capability.NewAssembler(nil)
	}
	if d.Runtime == nil {
		d.Runtime = NoRuntime{}
	}
	if d.Prober == nil {
		d.Prober = NoProber{}
	}
	if d.Text.IsZero() {
		d.Text = i18n.New(i18n.LocaleZH)
	}
	// The default memory keeps global memory in the home directory the
	// coordinator reads, when that home is a directory.
	homeDir := t.TempDir()
	if dir, ok := d.Home.(home.Dir); ok {
		homeDir = dir.Path
	}
	if d.Home == nil {
		d.Home = home.Dir{Path: homeDir}
	}
	if d.Skills == nil {
		d.Skills = &skills.Live{}
	}
	if d.Projects == nil {
		d.Projects = project.Open(book)
	}
	if d.Memory == nil {
		dir := filepath.Join(t.TempDir(), "memory")
		d.Memory = memory.NewService(memory.NewMarkdown(homeDir, dir), filepath.Join(dir, "audit.jsonl"))
	}
	if d.Attempts == nil {
		d.Attempts = attempt.New(book)
	}
	if d.Intents == nil {
		d.Intents = intent.New(book)
	}
	if d.Tasks == nil {
		d.Tasks, err = task.OpenLedger(book)
		must(err)
	}
	if d.Executions == nil {
		d.Executions = execution.New(t.Context(), d.Tasks)
	}
	if d.Artifacts == nil {
		// Bound to the registry the way the application binds its store,
		// so a landing runs in the source task's execution scope.
		artifacts := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, d.Projects, artifact.LocalNodes{Dir: t.TempDir()})
		artifacts.SetExecution(d.Executions)
		d.Artifacts = artifacts
	}
	if d.Schedules == nil {
		d.Schedules, err = schedule.OpenLedger(book)
		must(err)
	}
	if d.Plans == nil {
		d.Plans, err = plan.OpenLedger(book)
		must(err)
	}
	return *d
}

// New builds a coordinator from Deps(t, opts...) and wires it with
// Callbacks(the options' Callbacks).
func New(t testing.TB, opts ...Option) *turn.Coordinator {
	t.Helper()
	o := options(opts)
	c := build(t, o)
	c.Wire(Callbacks(o.Callbacks))
	return c
}

// Unwired builds a coordinator from Deps(t, opts...) and leaves Wire to the
// test, for callbacks that are built from the coordinator itself.
func Unwired(t testing.TB, opts ...Option) *turn.Coordinator {
	t.Helper()
	return build(t, options(opts))
}

func build(t testing.TB, o Options) *turn.Coordinator {
	t.Helper()
	c, err := turn.New(fill(t, o))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ErrNoCallback is what a callback Callbacks filled answers with.
var ErrNoCallback = errors.New("turntest: nothing behind this callback")

// Callbacks fills every callback turn.Coordinator.Wire requires that cb
// leaves unset: a supervisor that plans nothing and has no runs, a
// workspace attach that refuses, a resumer that accepts, and a notifier and
// dispatcher that drop what they are given.
//
// fillCallbacks in the turn package's own tests fills Callbacks the same
// way; change both together.
func Callbacks(cb turn.Callbacks) turn.Callbacks {
	if cb.Supervisor == nil {
		cb.Supervisor = IdleSupervisor{}
	}
	if cb.WorkspaceAttach == nil {
		cb.WorkspaceAttach = func(context.Context, string, string) error { return ErrNoCallback }
	}
	if cb.Notifier == nil {
		cb.Notifier = func(turn.TaskNotice) {}
	}
	if cb.Resumer == nil {
		cb.Resumer = func(turn.TaskResume) error { return nil }
	}
	if cb.ResumeDispatcher == nil {
		cb.ResumeDispatcher = func(turn.TaskResume) {}
	}
	return cb
}

// IdleSupervisor plans nothing and has no runs to resume.
type IdleSupervisor struct{}

func (IdleSupervisor) Plan(context.Context, planner.Request) (plan.Plan, error) {
	return plan.Plan{}, ErrNoCallback
}
func (IdleSupervisor) Execute(context.Context, plan.Plan) (exec.Outcome, error) {
	return exec.Outcome{}, ErrNoCallback
}
func (IdleSupervisor) Name() string                                       { return "idle" }
func (IdleSupervisor) PrepareRecovery(context.Context) error              { return nil }
func (IdleSupervisor) OpenRuns(context.Context) ([]exec.RunRecord, error) { return nil, nil }
func (IdleSupervisor) Resume(context.Context, exec.RunRecord) (exec.Outcome, error) {
	return exec.Outcome{}, ErrNoCallback
}

// ErrNoRuntime is what NoRuntime answers every session request with.
var ErrNoRuntime = errors.New("turntest: no agent runtime")

// NoRuntime is a runtime that starts no agent.
type NoRuntime struct{}

func (NoRuntime) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return nil, ErrNoRuntime
}

func (NoRuntime) CloseSession(context.Context, harness.Placement, string) error { return nil }

func (NoRuntime) SupportsHTTPMCP(context.Context, harness.Placement) (bool, error) {
	return false, ErrNoRuntime
}

// NoProber probes nothing and finds nothing.
type NoProber struct{}

func (NoProber) Probe(context.Context, string, string) error { return nil }

func (NoProber) ProbeAll(context.Context) []models.Result { return nil }

// ErrIdleCoordinator is what IdleCoordinator's SessionSetup and
// InitializeConversation answer with.
var ErrIdleCoordinator = errors.New("turntest: the coordinator is idle")

// IdleCoordinator stands in for a coordinator that runs no turn and knows
// no conversation: it has no context, setup, suggestions or verbs to give,
// parses a line by its syntax alone, starts no conversation, has no
// session to reset and refuses no scheduled run. A fake embeds it and
// overrides what its test is about. Handle panics, so a fake whose test
// runs a turn overrides Handle.
type IdleCoordinator struct{}

func (IdleCoordinator) Handle(context.Context, turn.Request) (turn.Result, error) {
	panic("turntest: IdleCoordinator runs no turn; a test that needs one embeds it and overrides Handle")
}

func (IdleCoordinator) Context(context.Context, string) (turn.Context, error) {
	return turn.Context{}, nil
}

func (IdleCoordinator) SessionSetup(context.Context, string, string) (turn.Setup, error) {
	return turn.Setup{}, ErrIdleCoordinator
}

func (IdleCoordinator) Suggest(context.Context, string, string) []turn.Suggestion { return nil }

func (IdleCoordinator) VerbsFor(context.Context) []turn.Verb { return nil }

func (IdleCoordinator) ParseInput(input string) (string, turn.ParsedInput) {
	return turn.ParseAddressedInput(input)
}

func (IdleCoordinator) InitializeConversation(context.Context, string, string, string) error {
	return ErrIdleCoordinator
}

func (IdleCoordinator) ResetConversationSessions(context.Context, string) error { return nil }

func (IdleCoordinator) ValidateScheduled(context.Context, string, string, string) error { return nil }
