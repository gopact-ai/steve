//go:build linux || darwin

package lifecycle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/view"
)

// An agent the hub runs on its own machine that exits mid-prompt fails the
// attempt: the transport ends what the agent left in its process group,
// which confirms its stop, and the attempt is not held as one whose agent
// may still be writing.
func TestRunFailsTheAttemptOfAHubAgentThatExitsDuringThePrompt(t *testing.T) {
	dir := t.TempDir()
	mock := filepath.Join(dir, "mockagent")
	build := exec.Command("go", "build", "-o", mock, "github.com/gopact-ai/steve/cmd/mockagent")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, out)
	}
	pidFile := filepath.Join(dir, "agent.pid")
	sessions, err := harness.NewManager(map[string]harness.Config{"mock": {
		Command: "/bin/sh", Args: []string{"-c", `echo $$ > "$2"; exec "$1"`, "agent", mock, pidFile}, Permission: "deny",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sessions.Stop)
	w := newWorld("")
	o := w.options()
	o.Sessions, o.At, o.Workdir, o.Prompt = sessions, harness.Placement{Harness: "mock"}, t.TempDir(), "ignore-cancel"
	var once sync.Once
	o.Observe = func(p view.Progress) {
		if !strings.Contains(p.Answer, "still running") {
			return
		}
		once.Do(func() {
			raw, _ := os.ReadFile(pidFile)
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(raw))); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	res, err := Run(ctx, o)
	if err == nil || errors.Is(err, harness.ErrStopUnconfirmed) || res.Unsettled || res.Record.Unsettled || res.Record.State != attempt.Failed {
		t.Fatalf("attempt of an agent that exited: state=%s unsettled=%v err=%v", res.Record.State, res.Unsettled, err)
	}
}
