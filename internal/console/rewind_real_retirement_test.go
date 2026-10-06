package console

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"github.com/gopact-ai/steve/internal/view"
)

type rewindRetirementRuntime struct {
	turntest.NoRuntime
	closeErr error
	opens    int
	prompts  int
}
type rewindRetirementRunner struct {
	runtime *rewindRetirementRuntime
	id      string
}

func (r *rewindRetirementRunner) ID() string { return r.id }
func (r *rewindRetirementRunner) Prompt(context.Context, string, func(view.Progress)) (string, []string, error) {
	r.runtime.prompts++
	return "real coordinator answer", nil, nil
}
func (*rewindRetirementRunner) Cancel(context.Context) error { return nil }
func (*rewindRetirementRunner) Abort()                       {}
func (r *rewindRetirementRuntime) OpenSession(_ context.Context, _ harness.Placement, id, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	if id == "" {
		r.opens++
		id = "ns_rewind_one"
		if r.opens > 1 {
			id = "ns_rewind_two"
		}
	}
	return &rewindRetirementRunner{runtime: r, id: id}, nil
}
func (r *rewindRetirementRuntime) CloseSession(context.Context, harness.Placement, string) error {
	return r.closeErr
}

func rewindRetirementService(t *testing.T) (*Service, *rewindRetirementRuntime, *state.Store) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &rewindRetirementRuntime{}
	c := turntest.New(t, func(o *turntest.Options) {
		o.Ledger = book
		o.Catalog = catalog
		o.Store = store
		o.Runtime = runtime
		o.Projects = projects
		o.DefaultProject = "p"
		o.Owner = "owner"
		o.Timeout = time.Minute
	})
	s := New(c, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	return s, runtime, store
}

func TestRewindThroughRealCoordinatorPreservesTranscriptAfterCloseFailure(t *testing.T) {
	s, runtime, store := rewindRetirementService(t)
	for _, line := range []string{"first user line", "second user line"} {
		if _, err := s.Send(t.Context(), "main", line); err != nil {
			t.Fatal(err)
		}
	}
	before := s.Replies("main")
	beforeState := store.Conversation("console:main")
	prompts := runtime.prompts
	failure := errors.New("test close response lost")
	runtime.closeErr = failure
	if _, err := s.Submit(t.Context(), consoleapi.Submission{Conversation: "main", Input: "edited first line", CommandID: "rewind-original", RewindTo: sentIDs(before)[0]}); !errors.Is(err, failure) {
		t.Fatalf("real reset swallowed close failure: %v", err)
	}
	if !reflect.DeepEqual(before, s.Replies("main")) {
		t.Fatal("failed native close truncated the transcript")
	}
	if !reflect.DeepEqual(beforeState, store.Conversation("console:main")) || len(store.OwedCloses()) != 0 {
		t.Fatal("unknown close forgot session or invented an obligation")
	}
	if runtime.prompts != prompts {
		t.Fatal("failed rewind submitted a replacement prompt")
	}
	runtime.closeErr = nil
	if _, err := s.Submit(t.Context(), consoleapi.Submission{Conversation: "main", Input: "edited first line", CommandID: "rewind-original", RewindTo: sentIDs(before)[0]}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s, "console:main")
	if runtime.prompts != prompts+1 {
		t.Fatal("retry did not submit exactly one new explicit prompt")
	}
}

func TestRewindThroughRealCoordinatorKeepsUndispatchedCloseAuthority(t *testing.T) {
	s, runtime, store := rewindRetirementService(t)
	if _, err := s.Send(t.Context(), "main", "first user line"); err != nil {
		t.Fatal(err)
	}
	before := s.Replies("main")
	original := store.Conversation("console:main").Sessions["worker"]
	runtime.closeErr = &nodewire.SessionNotDispatched{Cause: errors.New("test node offline")}
	if _, err := s.Submit(t.Context(), consoleapi.Submission{Conversation: "main", Input: "edited first line", CommandID: "rewind-original", RewindTo: sentIDs(before)[0]}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s, "console:main")
	owed := store.OwedCloses()
	if len(owed) != 1 || owed[0].UpstreamID != original.UpstreamID || owed[0].HarnessID != original.HarnessID || owed[0].NodeID != original.NodeID {
		t.Fatalf("fresh context lost old native close: %+v", owed)
	}
	live := store.Conversation("console:main").Sessions["worker"]
	if live.UpstreamID == "" || live.UpstreamID == original.UpstreamID {
		t.Fatal("rewind reused the abandoned old context")
	}
	if runtime.prompts != 2 {
		t.Fatal("rewind duplicated explicit input")
	}
}
