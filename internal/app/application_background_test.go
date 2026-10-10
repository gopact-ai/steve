package app

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"sync/atomic"
	"testing"
)

func TestApplicationBackgroundJoinsOldGenerationAndRejectsLateCallbacks(t *testing.T) {
	background := newApplicationBackground(t.Context())
	started := make(chan struct{})
	var finished atomic.Bool
	background.Go(func(ctx context.Context) { close(started); <-ctx.Done(); finished.Store(true) })
	<-started
	background.Close()
	if !finished.Load() {
		t.Fatal("generation closed while its background worker was still running")
	}
	background.Go(func(context.Context) { t.Error("old callback started work after generation close") })
	background.Close()
}

func TestLazyHubObservationDoesNotStartConfiguredAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, count := range []int{100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			dir := t.TempDir()
			program, starts := filepath.Join(dir, "agent"), filepath.Join(dir, "starts")
			tool := filepath.Join(dir, "host-tool")
			toolStarts := filepath.Join(dir, "tool-starts")
			write := func(path, counter string) {
				t.Helper()
				body := "#!/bin/sh\necho start >> '" + counter + "'\necho 'fixture 1.2.3'\n"
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(program, starts)
			write(tool, toolStarts)
			cfg := &config.Config{Harnesses: map[string]config.Harness{}}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			cfg.Gateway.Tools = []string{filepath.Base(tool), filepath.Base(program)}
			for i := range count {
				cfg.Harnesses[fmt.Sprintf("custom-%04d", i)] = config.Harness{Command: program, Args: []string{"acp", "", "含 空格"}, Env: []string{"MODE=ordinary"}}
			}
			for _, id := range []string{"codex", "claude-code", "grok", "kimi"} {
				cfg.Harnesses[id] = config.Harness{Command: program, Adapter: "codex-acp"}
			}
			store := admin.NewConfigStore(cfg)
			observation, close := newLocalObservation(t.Context(), store)
			t.Cleanup(close)
			wait := func(after time.Time) {
				t.Helper()
				until := time.Now().Add(4 * time.Second)
				for time.Now().Before(until) {
					result, ok := observation.Launch.Lookup(tool)
					if ok && result.At.After(after) {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
				t.Fatal("hub host tool did not complete its probe")
			}
			wait(time.Time{})
			for i := range 2 {
				before, _ := observation.Launch.Lookup(tool)
				if err := store.Update(func(cfg *config.Config) error {
					cfg.Gateway.Capabilities = []string{fmt.Sprintf("wake-%d", i)}
					return nil
				}, func(*config.Config) error { return nil }); err != nil {
					t.Fatal(err)
				}
				observation.Launch.Wake()
				wait(before.At)
			}
			if raw, err := os.ReadFile(starts); err == nil {
				t.Errorf("%d hub harnesses started agent processes without a job: %q", count, raw)
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if raw, err := os.ReadFile(toolStarts); err != nil || strings.Count(string(raw), "\n") < 3 {
				t.Fatalf("ordinary hub host tool lost probes: %q %v", raw, err)
			}
		})
	}
}

// The real spawn tests cover behavior; this guard prevents a future startup
// hookup from bypassing the host-tool worker and dropping agent argv/env again.
func TestLazyObservationStartupHasNoBareCommandOrModelProbeBypass(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "application_background.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var startup *ast.FuncDecl
	for _, declaration := range source.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "newLocalObservation" {
			startup = function
		}
	}
	if startup == nil {
		t.Fatal("missing local observation startup")
	}
	calls := 0
	ast.Inspect(startup.Body, func(n ast.Node) bool {
		if selector, ok := n.(*ast.SelectorExpr); ok && (selector.Sel.Name == "Harnesses" || selector.Sel.Name == "Command" || selector.Sel.Name == "Probe") {
			t.Errorf("startup bypasses configured host-tool launch boundary: %s", selector.Sel.Name)
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				switch selector.Sel.Name {
				case "RunLaunchProbe":
					calls++
				case "NewLocalObservation", "NewLaunchProbe", "Go":
				default:
					t.Errorf("unexpected startup execution path: %s", selector.Sel.Name)
				}
			}
		}
		return true
	})
	if calls != 1 {
		t.Fatalf("startup uses host-tool launch worker %d times, want 1", calls)
	}
}
