package app

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	steveexec "github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// Exercise the assembled After, not a copy of its control flow. The only
// process is a temporary mock harness; no listeners or remote services run.
func TestReadModelSkillsAfterConvergesLocallyDespiteRemoteError(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build mock harness: %v\n%s", err, out)
	}
	for _, mode := range []string{"pending", "canceled", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			book, err := ledger.Open(filepath.Join(root, "ledger"), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { book.Close() })
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			background := newApplicationBackground(ctx)
			t.Cleanup(background.Close)
			manager, err := harness.NewManager(map[string]harness.Config{"mock": {Command: bin, Permission: "deny"}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(manager.Stop)
			at := harness.Placement{Harness: "mock"}
			runner, err := manager.OpenSession(t.Context(), at, "", root, nil)
			if err != nil {
				t.Fatal(err)
			}
			nodes := node.NewRegistry("test-hub", map[string]node.Config{"offline": {
				DialContext: func(context.Context, string) (net.Conn, error) {
					t.Error("skills save must not dial the offline node")
					return nil, errors.New("offline")
				},
			}})
			t.Cleanup(nodes.Close)
			m, err := skills.Open(skills.DefaultPath(root))
			if err != nil {
				t.Fatal(err)
			}
			live := &skills.Live{Map: m, Dests: []string{filepath.Join(root, "runtime")}}
			if err := live.Apply(); err != nil {
				t.Fatal(err)
			}
			_, err = assembleReadModel(&assemblyInput{},
				&runtimeValues{book: book, cfg: &config.Config{}, ctx: ctx, background: background, live: live, manager: manager},
				&ledgerValues{}, &fleetValues{nodes: nodes, observation: &adminsvc.LocalObservation{}},
				&modelsValues{}, &executionValues{}, &plansValues{stepRunner: &steveexec.AgentRunner{}})
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "alpha")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "canceled":
				cancel()
			case "stopped":
				manager.Stop()
			}
			// SetSkill owns the real skills admission lock until After returns.
			service := &adminsvc.Service{LiveSkills: live, Coordinator: turntest.New(t, func(o *turntest.Options) { o.Runtime = manager })}
			after := live.After
			live.After = func() error {
				if release, ok := service.Coordinator.SkillsLock(); ok {
					release()
					t.Error("After ran outside skills admission")
				}
				return after()
			}
			err = service.SetSkill(t.Context(), target, true)
			want := adminsvc.ErrSkillsPending
			if mode == "canceled" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("lost remote error: %v, want %v", err, want)
			}
			if mode == "stopped" && !strings.Contains(err.Error(), "harness manager is stopped") {
				t.Fatalf("lost local restart error: %v", err)
			}
			if _, _, err := runner.Prompt(t.Context(), "must not reuse old host", nil); !errors.Is(err, acphost.ErrClosed) {
				t.Fatalf("old host remains usable after materialization: %v", err)
			}
			_, err = manager.OpenSession(t.Context(), at, "", root, nil)
			if mode == "stopped" {
				if err == nil || !strings.Contains(err.Error(), "harness manager is stopped") {
					t.Fatalf("stopped manager admitted an open: %v", err)
				}
			} else if err != nil {
				t.Fatalf("new host cannot open after local convergence: %v", err)
			}
		})
	}
}
