package turn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/view"
)

type copyWritingRuntime struct {
	mu           sync.Mutex
	dirs         []string
	upstreams    []string
	firstReady   chan struct{}
	firstRelease chan struct{}
}

func (m *copyWritingRuntime) OpenSession(_ context.Context, _ harness.Placement, upstream, dir string, _ []acp.MCPServer) (harness.Runner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs = append(m.dirs, dir)
	m.upstreams = append(m.upstreams, upstream)
	r := &copyWritingRunner{fakeRunner: &fakeRunner{id: fmt.Sprintf("copy-session-%d", len(m.dirs))}, dir: dir}
	if len(m.dirs) == 1 {
		r.ready, r.release = m.firstReady, m.firstRelease
	}
	return r, nil
}
func (*copyWritingRuntime) CloseSession(context.Context, harness.Placement, string) error { return nil }
func (*copyWritingRuntime) SupportsHTTPMCP(context.Context, harness.Placement) (bool, error) {
	return false, nil
}

type copyWritingRunner struct {
	*fakeRunner
	dir     string
	ready   chan struct{}
	release chan struct{}
}

func (r *copyWritingRunner) Prompt(ctx context.Context, _ string, _ func(view.Progress)) (string, []string, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if r.ready != nil {
		close(r.ready)
		select {
		case <-r.release:
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
	raw, err := os.ReadFile(filepath.Join(r.dir, "original"))
	if err != nil {
		return "", nil, err
	}
	next := append(append([]byte(nil), raw...), []byte("accepted continuation\n")...)
	if err := os.WriteFile(filepath.Join(r.dir, "original"), next, 0600); err != nil {
		return "", nil, err
	}
	return "continued in the copy", nil, nil
}

func TestTwoConversationsContinueTheSharedRecoveryThroughRealTurnLifecycle(t *testing.T) {
	c, p, source, workspace := sharedCopy(t)
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Node: "node", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &copyWritingRuntime{}
	c.catalog, c.runtime = catalog, runtime
	frozen, _ := c.tasks.Get(source.TaskID)
	for n, conversation := range []string{"console:new-one", "console:new-two"} {
		if _, err := c.projects.Bind(t.Context(), conversation, p.ID, "owner"); err != nil {
			t.Fatal(err)
		}
		result, err := c.Handle(t.Context(), Request{Channel: "console", ConversationID: conversation, MessageID: fmt.Sprintf("web-copy-%d", n), SenderOpenID: "owner", Input: "@worker continue this project"})
		if err != nil {
			t.Fatalf("continuation %d: %+v %v", n, result, err)
		}
		if result.Text != "continued in the copy" {
			t.Fatalf("reply=%+v", result)
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.dirs) != 2 || runtime.dirs[0] != workspace.Path || runtime.dirs[1] != workspace.Path || runtime.upstreams[0] != "" || runtime.upstreams[1] != "" {
		t.Fatalf("new tasks did not open fresh sessions on the shared directory: %+v %+v", runtime.dirs, runtime.upstreams)
	}
	raw, err := os.ReadFile(filepath.Join(p.Home.Path, "original"))
	if err != nil || string(raw) != "named base\n" {
		t.Fatalf("continuation wrote original directory: %q %v", raw, err)
	}
	raw, err = os.ReadFile(filepath.Join(workspace.Path, "original"))
	if err != nil || string(raw) != "named base\naccepted continuation\naccepted continuation\n" {
		t.Fatalf("second conversation lost accepted work: %q %v", raw, err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Head.Version != 3 || len(episode.Head.Sources) != 2 || episode.Producer != nil {
		t.Fatalf("head publication missing: %+v %v", episode, err)
	}
	old, _ := c.tasks.Get(source.TaskID)
	if !sameAbandonJSON(frozen, old) {
		t.Fatal("new work changed frozen source accounting")
	}
	for _, conversation := range []string{"console:new-one", "console:new-two"} {
		tracked := c.tasks.List(conversation)
		if len(tracked) != 1 || len(tracked[0].Attempts) != 1 || tracked[0].Attempts[0].Open() {
			t.Fatalf("new execution accounting is not settled: %+v", tracked)
		}
	}
}

func TestConcurrentConversationsWaitForTheSharedRecoveryWriter(t *testing.T) {
	c, p, source, workspace := sharedCopy(t)
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Node: "node", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &copyWritingRuntime{firstReady: make(chan struct{}), firstRelease: make(chan struct{})}
	c.catalog, c.runtime = catalog, runtime
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	var workers sync.WaitGroup
	first := make(chan error, 1)
	second := make(chan error, 1)
	defer func() {
		cancel()
		select {
		case <-runtime.firstRelease:
		default:
			close(runtime.firstRelease)
		}
		workers.Wait()
	}()
	for _, conversation := range []string{"console:first", "console:second"} {
		if _, err := c.projects.Bind(ctx, conversation, p.ID, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, err := c.Handle(ctx, Request{Channel: "console", ConversationID: "console:first", MessageID: "web-first", SenderOpenID: "owner", Input: "@worker first"})
		first <- err
	}()
	select {
	case <-runtime.firstReady:
	case err := <-first:
		t.Fatalf("first failed before prompt: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waiting := make(chan struct{}, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, err := c.Handle(ctx, Request{Channel: "console", ConversationID: "console:second", MessageID: "web-second", SenderOpenID: "owner", Input: "@worker second", OnStage: func(stage view.Stage) {
			if stage == view.StageAwaitSnapshot {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
		}})
		second <- err
	}()
	select {
	case <-waiting:
	case err := <-second:
		t.Fatalf("second conversation did not wait for the shared writer: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	runtime.mu.Lock()
	opened := len(runtime.dirs)
	runtime.mu.Unlock()
	if opened != 1 {
		t.Fatal("a second native session opened before the first writer finished")
	}
	close(runtime.firstRelease)
	for _, done := range []chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	episode, err := c.attempts.WorkspaceRecovery(ctx, source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Head.Version != 3 {
		t.Fatalf("serialized inputs lost an accepted head: %+v %v", episode, err)
	}
	raw, err := os.ReadFile(filepath.Join(workspace.Path, "original"))
	if err != nil || string(raw) != "named base\naccepted continuation\naccepted continuation\n" {
		t.Fatalf("queued writer overwrote the first output: %q %v", raw, err)
	}
}
