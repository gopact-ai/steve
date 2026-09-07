package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

type diskSessionAuthority struct{ dir string }

func (a diskSessionAuthority) AuthorizeNodeSession(_ context.Context, principal string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, _ string) error {
	raw, err := os.ReadFile(filepath.Join(a.dir, "authority"))
	if err != nil {
		return err
	}
	if principal != "cluster-1" || string(raw) != fmt.Sprint(authority.CoordinatorEpoch) || authority.WriterGeneration != authority.CoordinatorEpoch || binding.NodeID != "worker" || binding.AttemptID != "attempt-1" {
		return fmt.Errorf("test committed activation differs")
	}
	return nil
}

type subprocessSessionReceipt struct{ ID, Question, Output string }

// TestNodeSessionSubprocess is an isolated worker/client process entrypoint.
// It runs only the repository mock agent and state under the test's TempDir.
func TestNodeSessionSubprocess(t *testing.T) {
	role := os.Getenv("STEVE_SESSION_TEST_ROLE")
	if role == "" {
		return
	}
	dir := os.Getenv("STEVE_SESSION_TEST_DIR")
	if role == "worker" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s := NewServer(ServerConfig{Name: "worker", Token: "session-process-test", StateDir: filepath.Join(dir, "state"), WorkspaceRoot: filepath.Join(dir, "workspace"), Harnesses: map[string]HarnessSpec{"mock": {Command: os.Getenv("STEVE_SESSION_TEST_AGENT")}}, SessionAuthorizer: diskSessionAuthority{dir}, Listener: listener})
		if err := (&ledger.FileDocument{Path: filepath.Join(dir, "address")}).Save([]byte(listener.Addr().String())); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
						cancel()
						return
					}
				}
			}
		}()
		if err := s.Serve(ctx); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, "address"))
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: string(raw), Token: "session-process-test"}})
	defer registry.Close()
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetTransports(registry)
	defer manager.Stop()
	req := nodeSessionRequest("open")
	binding := harness.NodeSessionContext{Authority: req.Authority, Binding: req.Binding, CommandID: "subprocess-input"}
	if role == "second" {
		binding.Authority.CoordinatorEpoch = 2
		binding.Authority.WriterGeneration = 2
		binding.Authority.CoordinatorNodeID = "hub-b"
	}
	ctx, cancel := context.WithTimeout(harness.WithNodeSession(t.Context(), binding), 30*time.Second)
	defer cancel()
	at := harness.Placement{Node: "worker", Harness: "mock"}
	if role == "first" {
		runner, err := manager.OpenSession(ctx, at, "", filepath.Join(dir, "workspace"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = runner.(harness.TurnRunner).PromptTurn(ctx, "askme", nil, nil, func(ctx context.Context, q view.Question) (view.Answer, error) {
			raw, _ := json.Marshal(subprocessSessionReceipt{ID: runner.ID(), Question: q.RequestID})
			if err := (&ledger.FileDocument{Path: filepath.Join(dir, "accepted.json")}).Save(raw); err != nil {
				return view.Answer{}, err
			}
			<-ctx.Done()
			return view.Answer{}, ctx.Err()
		}, nil)
		if err != nil && ctx.Err() == nil {
			t.Fatal(err)
		}
		return
	}
	raw, err = os.ReadFile(filepath.Join(dir, "accepted.json"))
	if err != nil {
		t.Fatal(err)
	}
	var previous subprocessSessionReceipt
	if err := json.Unmarshal(raw, &previous); err != nil {
		t.Fatal(err)
	}
	runner, err := manager.AttachRetainedSession(ctx, at, previous.ID, filepath.Join(dir, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	output, _, err := runner.ResumeTurn(ctx, nil, func(_ context.Context, q view.Question) (view.Answer, error) {
		if q.RequestID != previous.Question {
			t.Error("original question replaced")
		}
		return view.Answer{Value: "Blue"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	previous.Output = output
	raw, _ = json.Marshal(previous)
	if err := (&ledger.FileDocument{Path: filepath.Join(dir, "completed.json")}).Save(raw); err != nil {
		t.Fatal(err)
	}
}

func waitSessionTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("test process did not produce %s", filepath.Base(path))
}

func TestNodeSessionContinuesAfterCoordinatorProcessIsKilled(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "authority"), []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	agent := buildMockAgent(t)
	start := func(role string) *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=^TestNodeSessionSubprocess$", "-test.timeout=40s")
		cmd.Env = append(os.Environ(), "STEVE_SESSION_TEST_ROLE="+role, "STEVE_SESSION_TEST_DIR="+dir, "STEVE_SESSION_TEST_AGENT="+agent)
		log, err := os.Create(filepath.Join(dir, role+".log"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { log.Close() })
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	worker := start("worker")
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(dir, "stop"), nil, 0600)
		done := make(chan error, 1)
		go func() { done <- worker.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker exited: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = worker.Process.Kill()
			<-done
			t.Error("worker could not shut down")
		}
	})
	waitSessionTestFile(t, filepath.Join(dir, "address"))
	first := start("first")
	waitSessionTestFile(t, filepath.Join(dir, "accepted.json"))
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	if err := (&ledger.FileDocument{Path: filepath.Join(dir, "authority")}).Save([]byte("2")); err != nil {
		t.Fatal(err)
	}
	second := start("second")
	if err := second.Wait(); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "second.log"))
		t.Fatalf("replacement coordinator: %v\n%s", err, log)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "completed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt subprocessSessionReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt.Output, "accept:Blue") {
		t.Fatalf("original node execution did not continue: %+v", receipt)
	}
}
