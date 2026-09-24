// Package turntest builds turn coordinators for tests in other packages.
// Everything a test does not set is a real store or service on a
// temporary ledger, so a test names only what it cares about.
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
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
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
}

// Option sets part of Options.
type Option func(*Options)

// Deps is turn.Deps with every dependency the options leave unset filled:
// an empty agent catalog, a runtime that starts no agent, an empty home and
// skill set, the Chinese catalog, and every store on the options' ledger.
//
// fillDeps in the turn package's own tests fills Deps the same way, since
// those tests cannot import this package; change both together.
func Deps(t testing.TB, opts ...Option) turn.Deps {
	t.Helper()
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
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
	if d.Text.IsZero() {
		d.Text = i18n.New(i18n.LocaleZH)
	}
	homeDir := t.TempDir()
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
	return *d
}

// New builds a coordinator from Deps(t, opts...).
func New(t testing.TB, opts ...Option) *turn.Coordinator {
	t.Helper()
	c, err := turn.New(Deps(t, opts...))
	if err != nil {
		t.Fatal(err)
	}
	return c
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
