package harness

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/pihome"
)

func TestRetiredHarnessesNeverStart(t *testing.T) {
	m, err := NewManager(map[string]Config{"kimi": {Command: "/bin/sh", Permission: "read"}, "grok": {Command: "/bin/sh", Permission: "auto"}, "dsh": {Command: "/bin/sh", Permission: "write"}})
	if err != nil {
		t.Fatalf("a configuration naming retired tools must still load: %v", err)
	}
	t.Cleanup(m.Stop)
	for id, name := range map[string]string{"kimi": "Kimi Code", "grok": "Grok", "dsh": "DeepSeek Harness"} {
		if _, err := m.OpenSession(t.Context(), Placement{Harness: id}, "", t.TempDir(), nil); !errors.Is(err, ErrRetired) || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: %v", id, err)
		}
		if _, err := m.SupportsHTTPMCP(t.Context(), Placement{Harness: id}); !errors.Is(err, ErrRetired) {
			t.Fatalf("%s probe started a process: %v", id, err)
		}
	}
	if _, ok := Retired("codex"); ok {
		t.Fatal("codex reported as retired")
	}
}

// pi asks for no approval, so a read-only policy is held by the home it is
// started from; a home that does not hold it never starts.
func TestPiLaunchFollowsItsPolicy(t *testing.T) {
	base := t.TempDir()
	if err := pihome.Prepare(base, ""); err != nil {
		t.Fatal(err)
	}
	steve := []string{pihome.EnvHomes + "=" + base, EnvPiAgentDir + "=" + filepath.Join(base, pihome.ReadOnly)}
	for policy, class := range map[string]string{"read": pihome.ReadOnly, "write": pihome.Full} {
		env, err := envForPolicy(steve, Pi, policy)
		if err != nil || !slices.Contains(env, EnvPiAgentDir+"="+filepath.Join(base, class)) {
			t.Fatalf("%s: %v %v", policy, env, err)
		}
	}
	m, err := NewManager(map[string]Config{Pi: {Command: "/bin/sh", Permission: "read", Env: []string{EnvPiAgentDir + "=" + t.TempDir()}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	if _, err := m.SupportsHTTPMCP(t.Context(), Placement{Harness: Pi}); !errors.Is(err, pihome.ErrUnenforced) {
		t.Fatalf("pi started read-only from a home that does not restrict it: %v", err)
	}
	if env, err := envForPolicy([]string{"X=1"}, Codex, "read"); err != nil || !slices.Equal(env, []string{"X=1"}) {
		t.Fatalf("codex env changed: %v %v", env, err)
	}
}
