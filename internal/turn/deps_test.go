package turn

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
)

// testDeps is a complete Deps over book, with an empty agent catalog and a
// runtime that opens nothing. It mirrors turntest, which this package's own
// tests cannot import.
func testDeps(t *testing.T, book *ledger.Ledger) Deps {
	t.Helper()
	catalog, err := agent.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	schedules, err := schedule.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	homeDir, stateDir := t.TempDir(), t.TempDir()
	return Deps{
		Catalog: catalog, Store: store, Assembler: capability.NewAssembler(nil), Runtime: noRuntime{},
		Text: i18n.New(i18n.LocaleZH), Home: home.Dir{Path: homeDir}, Skills: &skills.Live{},
		Projects: projects, Attempts: attempt.New(book),
		Artifacts: artifact.New(filepath.Join(stateDir, "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()}),
		Intents:   intent.New(book), Tasks: tasks, Executions: execution.New(t.Context(), tasks), Schedules: schedules,
		Memory: memory.NewService(memory.NewMarkdown(homeDir, filepath.Join(stateDir, "memory")), filepath.Join(stateDir, "memory", "audit.jsonl")),
	}
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

func TestNewRefusesEveryMissingDependency(t *testing.T) {
	_, err := New(Deps{})
	if err == nil {
		t.Fatal("New(Deps{}) succeeded")
	}
	for _, name := range []string{"Catalog", "Store", "Assembler", "Runtime", "Text", "Home", "Skills", "Projects",
		"Memory", "Attempts", "Artifacts", "Intents", "Executions", "Tasks", "Schedules"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("New(Deps{}) error %q does not name %s", err, name)
		}
	}
}

func TestNewRefusesAnInvalidChannelOwner(t *testing.T) {
	deps := testDeps(t, testLedger(t))
	deps.ChannelOwners = map[string]string{"console": "owner"}
	if _, err := New(deps); err == nil {
		t.Fatal("New accepted an owner for the console channel")
	}
}

func TestNewWiresEveryDependency(t *testing.T) {
	deps := testDeps(t, testLedger(t))
	deps.Owner, deps.Node, deps.DefaultProject, deps.HomeProject = "owner", "hub", "p", "home"
	deps.ChannelOwners = map[string]string{"feishu": "native-owner"}
	deps.Text = i18n.New(i18n.LocaleEN)
	c, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if c.catalog != deps.Catalog || c.store != deps.Store || c.assembler != deps.Assembler || c.runtime != deps.Runtime ||
		c.skills != deps.Skills || c.projects != deps.Projects || c.memory != deps.Memory || c.attempts != deps.Attempts ||
		c.artifacts != deps.Artifacts || c.intents != deps.Intents || c.executions != deps.Executions ||
		c.tasks != deps.Tasks || c.schedules != deps.Schedules {
		t.Fatal("New dropped a store or service")
	}
	if _, dir := c.home.(home.Dir); !dir || c.homePath != deps.Home.(home.Dir).Path || c.ownerOpenID != "owner" || c.node != "hub" ||
		c.defaultProject != "p" || c.homeProject != "home" || c.text.Locale() != i18n.LocaleEN {
		t.Fatal("New dropped an identity, placement or locale setting")
	}
	native, err := c.forChannel("feishu")
	if err != nil || native.ownerOpenID != "native-owner" {
		t.Fatalf("feishu owner = %v, %v", native, err)
	}
}
