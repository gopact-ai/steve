package turn

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

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
)

// fillDeps fills every dependency d leaves unset, the way turntest does
// for other packages: an empty agent catalog, a runtime that starts no
// agent, an empty home and skill set, the Chinese catalog, and every store
// on book. Stores built from another default follow what d sets.
//
// It repeats turntest.Deps, which imports this package and so cannot be
// used from its own tests; change both together.
func fillDeps(t *testing.T, book *ledger.Ledger, d *Deps) {
	t.Helper()
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
		d.Runtime = noRuntime{}
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
}

// testOption adjusts how buildCoordinator builds a coordinator.
type testOption func(*testBuild)

type testBuild struct {
	book     *ledger.Ledger
	lifetime context.Context
	set      []func(*Deps)
}

// onLedger opens every default store on book instead of a fresh ledger.
func onLedger(book *ledger.Ledger) testOption {
	return func(b *testBuild) { b.book = book }
}

// withDeps sets dependencies before the defaults fill the rest; later
// options win.
func withDeps(set func(*Deps)) testOption {
	return func(b *testBuild) { b.set = append(b.set, set) }
}

// withOwner sets the baseline owner identity.
func withOwner(owner string) testOption {
	return withDeps(func(d *Deps) { d.Owner = owner })
}

// withHome sets the baseline owner identity and the home it reads.
func withHome(owner string, loader home.Loader) testOption {
	return withDeps(func(d *Deps) { d.Owner, d.Home = owner, loader })
}

// withChannelOwner registers owner as the native owner of channel.
func withChannelOwner(channel, owner string) testOption {
	return withDeps(func(d *Deps) {
		if d.ChannelOwners == nil {
			d.ChannelOwners = map[string]string{}
		}
		d.ChannelOwners[channel] = owner
	})
}

// withTasks sets the task store and the node it records.
func withTasks(tasks *task.Store, node string) testOption {
	return withDeps(func(d *Deps) { d.Tasks, d.Node = tasks, node })
}

// withExecutionLifetime gives the coordinator an execution registry whose
// generation ends with lifetime instead of with the test, on whichever task
// store it is built with.
func withExecutionLifetime(lifetime context.Context) testOption {
	return func(b *testBuild) { b.lifetime = lifetime }
}

// buildCoordinator builds a coordinator through New from the options.
func buildCoordinator(t *testing.T, opts ...testOption) *Coordinator {
	t.Helper()
	var b testBuild
	for _, opt := range opts {
		opt(&b)
	}
	if b.book == nil {
		b.book = testLedger(t)
	}
	var deps Deps
	for _, set := range b.set {
		set(&deps)
	}
	if b.lifetime != nil && deps.Executions == nil {
		if deps.Tasks == nil {
			tasks, err := task.OpenLedger(b.book)
			if err != nil {
				t.Fatal(err)
			}
			deps.Tasks = tasks
		}
		deps.Executions = execution.New(b.lifetime, deps.Tasks)
	}
	fillDeps(t, b.book, &deps)
	c, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	testLedgers.Store(c.coordinatorState, b.book)
	t.Cleanup(func() { testLedgers.Delete(c.coordinatorState) })
	return c
}

// testLedgers is the ledger each coordinator buildCoordinator built opened
// its default stores on, so a restarted coordinator opens its own there.
var testLedgers sync.Map // *coordinatorState -> *ledger.Ledger

// ledgerOf is the ledger c was built on.
func ledgerOf(t *testing.T, c *Coordinator) *ledger.Ledger {
	t.Helper()
	book, ok := testLedgers.Load(c.coordinatorState)
	if !ok {
		t.Fatal("coordinator was not built by buildCoordinator")
	}
	return book.(*ledger.Ledger)
}

var errNoRuntime = errors.New("no agent runtime in this test")

// noRuntime refuses to start any agent.
type noRuntime struct{}

func (noRuntime) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return nil, errNoRuntime
}

func (noRuntime) CloseSession(context.Context, harness.Placement, string) error { return nil }

func (noRuntime) SupportsHTTPMCP(context.Context, harness.Placement) (bool, error) {
	return false, errNoRuntime
}

// Each entry of Deps.required names a Deps field, is refused alone when
// that field is zero, and fills the coordinator field of the same type
// named after it, lower-cased.
func TestNewRefusesEachRequiredDependencyAlone(t *testing.T) {
	var full Deps
	fillDeps(t, testLedger(t), &full)
	if _, err := New(full); err != nil {
		t.Fatal(err)
	}
	required := full.required()
	if len(required) == 0 {
		t.Fatal("Deps.required lists no dependency")
	}
	coordinator := reflect.TypeFor[Coordinator]()
	for _, dep := range required {
		t.Run(dep.name, func(t *testing.T) {
			field, ok := reflect.TypeFor[Deps]().FieldByName(dep.name)
			if !ok {
				t.Fatalf("Deps.required lists %s, which is no Deps field", dep.name)
			}
			first, size := utf8.DecodeRuneInString(dep.name)
			filled := string(unicode.ToLower(first)) + dep.name[size:]
			if target, ok := coordinator.FieldByName(filled); !ok || target.Type != field.Type {
				t.Fatalf("Deps.%s fills no coordinator field %s of type %v", dep.name, filled, field.Type)
			}
			without := full
			reflect.ValueOf(&without).Elem().FieldByIndex(field.Index).SetZero()
			_, err := New(without)
			if want := "turn: missing dependencies: " + dep.name; err == nil || err.Error() != want {
				t.Fatalf("New without %s: %v, want %q", dep.name, err, want)
			}
		})
	}
}

func TestNewRefusesAnInvalidChannelOwner(t *testing.T) {
	var deps Deps
	fillDeps(t, testLedger(t), &deps)
	deps.ChannelOwners = map[string]string{"console": "owner"}
	if _, err := New(deps); err == nil {
		t.Fatal("New accepted an owner for the console channel")
	}
}

func TestNewWiresEveryDependency(t *testing.T) {
	var deps Deps
	fillDeps(t, testLedger(t), &deps)
	deps.Owner, deps.Node, deps.DefaultProject, deps.HomeProject = "owner", "hub", "p", "home"
	deps.ChannelOwners = map[string]string{"feishu": "native-owner"}
	deps.Text = i18n.New(i18n.LocaleEN)
	deps.OfflineAfter = time.Minute
	guarded := errors.New("guarded")
	deps.ConsoleCompletionGuard = func(*ledger.Tx, map[string]bool, string, string) error { return guarded }
	deps.Nodes = &idleNodes{}
	c, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if c.nodes != deps.Nodes {
		t.Fatal("New dropped the nodes")
	}
	if c.catalog != deps.Catalog || c.store != deps.Store || c.assembler != deps.Assembler || c.runtime != deps.Runtime ||
		c.skills != deps.Skills || c.projects != deps.Projects || c.memory != deps.Memory || c.attempts != deps.Attempts ||
		c.artifacts != deps.Artifacts || c.intents != deps.Intents || c.executions != deps.Executions ||
		c.tasks != deps.Tasks || c.schedules != deps.Schedules {
		t.Fatal("New dropped a store or service")
	}
	if _, dir := c.home.(home.Dir); !dir || c.homePath != deps.Home.(home.Dir).Path || c.ownerOpenID != "owner" || c.node != "hub" ||
		c.defaultProject != "p" || c.homeProject != "home" || c.text.Locale() != i18n.LocaleEN || c.offlineAfter != time.Minute {
		t.Fatal("New dropped an identity, placement, locale or reminder setting")
	}
	if err := c.checkConsoleCompletionTx(nil, nil, "", ""); !errors.Is(err, guarded) {
		t.Fatalf("completion guard = %v, want the one Deps gave", err)
	}
	native, err := c.forChannel("feishu")
	if err != nil || native.ownerOpenID != "native-owner" {
		t.Fatalf("feishu owner = %v, %v", native, err)
	}
}

func TestNewReadsRuntimePolicyFromItsSources(t *testing.T) {
	var deps Deps
	fillDeps(t, testLedger(t), &deps)
	deps.Timeout = time.Hour
	fixed, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.promptTimeout() != time.Hour || fixed.autoResolves() {
		t.Fatalf("without sources: timeout %v, auto-resolve %v", fixed.promptTimeout(), fixed.autoResolves())
	}
	deps.TimeoutSource = func() time.Duration { return 19 * time.Minute }
	autoResolve := true
	deps.AutoResolveSource = func() bool { return autoResolve }
	live, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if live.promptTimeout() != 19*time.Minute || !live.autoResolves() {
		t.Fatalf("with sources: timeout %v, auto-resolve %v", live.promptTimeout(), live.autoResolves())
	}
	autoResolve = false
	if live.autoResolves() {
		t.Fatal("auto-resolve kept a value its source no longer gives")
	}
}

func TestFillDepsKeepsDefaultMemoryInTheCoordinatorsHome(t *testing.T) {
	chosen := t.TempDir()
	withChosen := Deps{Home: home.Dir{Path: chosen}}
	fillDeps(t, testLedger(t), &withChosen)
	var withDefault Deps
	fillDeps(t, testLedger(t), &withDefault)
	for name, deps := range map[string]Deps{"chosen": withChosen, "default": withDefault} {
		want := filepath.Join(deps.Home.(home.Dir).Path, home.FileMemory)
		if got := deps.Memory.Where(memory.Global); got != want {
			t.Errorf("%s home: global memory at %q, want %q", name, got, want)
		}
	}
}
