package harness

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
)

type testRemoteTransport struct {
	binary        string
	node, harness string
}

func (r *testRemoteTransport) Transport(node, harness string) acphost.Transport {
	r.node, r.harness = node, harness
	return acphost.LocalTransport{Command: r.binary}
}

func TestRemoteOnlyHarnessDefaultsToReadAndPreservesExplicitPolicy(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v %s", err, out)
	}
	for _, policy := range []string{"", "deny", "auto"} {
		t.Run("policy-"+policy, func(t *testing.T) {
			cfg := map[string]Config{}
			if policy != "" {
				cfg["remote-only"] = Config{Command: "must-not-execute-local-command", Env: []string{"LOCAL_CREDENTIAL=never-copy"}, Permission: policy}
			}
			manager, err := NewManager(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Stop()
			remote := &testRemoteTransport{binary: bin}
			manager.SetTransports(remote)
			runner, err := manager.OpenSession(t.Context(), Placement{Node: "worker", Harness: "remote-only"}, "", t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if remote.node != "worker" || remote.harness != "remote-only" {
				t.Fatal("remote placement was not used")
			}
			out, _, err := runner.Prompt(context.Background(), "perm", nil)
			if err != nil {
				t.Fatal(err)
			}
			want := "selected/reject"
			if policy == "auto" {
				want = "selected/allow"
			}
			if !strings.Contains(out, want) {
				t.Fatalf("policy=%q output=%q want %q", policy, out, want)
			}
		})
	}
}
