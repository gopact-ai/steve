package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nativehistory"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/turn"
)

func nativeImportAdminFixture(t *testing.T, bin string) (*Service, *state.Store, consoleapi.NativeImportRequest, string, *ledger.Ledger) {
	t.Helper()
	a, book := projectAdminFixture(t)
	server := startNativeImportNode(t, bin)
	a.Cfg.Nodes["node-test"] = config.Node{Addr: server.Addr(), Token: "test-node-token"}
	a.Cfg.Agents["importer"] = config.Agent{Node: "node-test", Harness: "codex"}
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	a.Catalog, _ = agent.NewCatalog(map[string]agent.Config{"importer": {Node: "node-test", Harness: "codex", Default: true}})
	a.Nodes = node.NewRegistry("hub-test", configbuild.NodeConfigs(a.Cfg))
	t.Cleanup(a.Nodes.Close)
	a.ClusterMode = true
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	a.Coordinator = turn.New(a.Catalog, store, nil, nil, time.Second)
	a.Coordinator.SetProjects(a.Projects, "p", "")
	a.Coordinator.SetIdentity("owner", nil)
	a.Console = console.New(nil, "owner", nil)
	if err := a.Console.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	source, work := t.TempDir(), t.TempDir()
	history := filepath.Join(source, "sessions/2026/09/14/rollout-import.jsonl")
	if err := os.MkdirAll(filepath.Dir(history), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": "imported-native", "cwd": work}})
	if err := os.WriteFile(history, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	src := nativehistory.Source{Harness: "codex", Home: source}
	list, err := a.NativeHistory(t.Context(), "node-test", src)
	if err != nil || len(list) != 1 {
		t.Fatalf("node source list=%+v %v", list, err)
	}
	req := consoleapi.NativeImportRequest{CommandID: "import-command", Agent: "importer", Source: src, NativeID: list[0].NativeID, Revision: list[0].Revision}
	return a, store, req, history, book
}

func TestNativeImportAutoAssociationRecoversAfterSourceDisappears(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v %s", err, raw)
	}
	for _, failure := range []string{"project-save", "receipt-save"} {
		t.Run(failure, func(t *testing.T) {
			a, store, req, history, book := nativeImportAdminFixture(t, bin)
			if failure == "project-save" {
				a.WriteConfig = func(string, *config.Config) error { return errors.New("project save unavailable") }
			} else {
				if _, err := book.DB().Exec(`CREATE TRIGGER reject_console_receipt BEFORE UPDATE ON bindings WHEN new.kind='console-store' BEGIN SELECT RAISE(ABORT, 'receipt save unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.ImportNativeHistory(t.Context(), "node-test", req); err == nil {
				t.Fatal("injected failure did not fail import")
			}
			if len(a.Console.Conversations()) != 0 {
				t.Fatal("failed import became visible")
			}
			conversation, _ := console.NativeImportConversation(req.CommandID)
			before, exists, err := a.Projects.Binding(t.Context(), conversation)
			if err != nil {
				t.Fatal(err)
			}
			if (failure == "receipt-save") != exists {
				t.Fatalf("unexpected intermediate binding: %+v %v", before, exists)
			}
			if err := os.Remove(history); err != nil {
				t.Fatal(err)
			}
			a.WriteConfig = nil
			if _, err := book.DB().Exec(`DROP TRIGGER IF EXISTS reject_console_receipt`); err != nil {
				t.Fatal(err)
			}
			got, err := a.ImportNativeHistory(t.Context(), "node-test", req)
			if err != nil {
				t.Fatalf("source-free retry failed: %v", err)
			}
			if got.Project == "" || got.Conversation != conversation || got.Reference.NativeID != req.NativeID {
				t.Fatalf("import=%+v", got)
			}
			session, ok := store.Conversation(conversation).Sessions[req.Agent]
			if !ok || session.NativeImport == nil || session.NativeImport.ID != got.Reference.ID || session.UpstreamID != "" {
				t.Fatalf("import did not install an unstarted session: %+v", session)
			}
			after, _, err := a.Projects.Binding(t.Context(), conversation)
			if err != nil || after.Version != 1 || exists && after != before {
				t.Fatalf("retry changed initial binding: %+v -> %+v %v", before, after, err)
			}
			if len(a.Cfg.Projects) != 3 || len(a.Console.Conversations()) != 1 {
				t.Fatal("retry duplicated project or conversation")
			}
			// A completed receipt is self-contained even after disconnecting the source.
			a.Nodes.Close()
			again, err := a.ImportNativeHistory(t.Context(), "node-test", req)
			if err != nil || again != got {
				t.Fatalf("completed receipt required source: %+v %v", again, err)
			}
		})
	}
}

func startNativeImportNode(t *testing.T, bin string) *node.Server {
	t.Helper()
	server := node.NewServer(node.ServerConfig{Name: "node-test", Token: "test-node-token", Listen: "127.0.0.1:0", StateDir: t.TempDir(), Harnesses: map[string]node.HarnessSpec{"codex": {Command: bin}}, SessionAuthorizer: node.CoordinatorSessionAuthorizer{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("node did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for strings.HasSuffix(server.Addr(), ":0") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if strings.HasSuffix(server.Addr(), ":0") {
		t.Fatal("node did not start")
	}
	return server
}
