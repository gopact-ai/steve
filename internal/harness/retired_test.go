package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func buildPiMock(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	// Each adapter process start is logged with the home it was given.
	starts := filepath.Join(dir, "starts")
	adapter := filepath.Join(dir, "pi-acp")
	script := "#!/bin/sh\necho \"$PI_CODING_AGENT_DIR $PI_ACP_PI_COMMAND\" >> '" + starts + "'\nexec '" + bin + "' \"$@\"\n"
	if err := os.WriteFile(adapter, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return adapter, starts
}

// envOf asks the session's own agent process for an environment variable.
func envOf(t *testing.T, ctx context.Context, r Runner, name string) string {
	t.Helper()
	out, _, err := r.Prompt(ctx, "mockenv "+name, nil)
	if err != nil {
		t.Fatalf("prompt %s: %v", name, err)
	}
	value, ok := strings.CutPrefix(strings.TrimSpace(out), "env "+name+"=")
	if !ok {
		t.Fatalf("unexpected answer %q", out)
	}
	return value
}

func startsOf(t *testing.T, path string) []string {
	t.Helper()
	raw, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// Narrowing a running pi's policy closes the process started from the full
// home: the session the next open returns runs in a process whose home and
// launcher are the read-only ones, a saved write session is not resumed, and
// the record names the class of that process.
func TestPiPolicyChangeNeverReusesTheOtherClassProcess(t *testing.T) {
	adapter, starts := buildPiMock(t)
	base := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	env := []string{pihome.EnvHomes + "=" + base, EnvPiAgentDir + "=" + pihome.Dir(base, pihome.ReadOnly)}
	full, readOnly := pihome.Dir(base, pihome.Full), pihome.Dir(base, pihome.ReadOnly)
	m, err := NewManager(map[string]Config{Pi: {Command: adapter, Permission: "write", Env: env}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	at := Placement{Harness: Pi}
	work := t.TempDir()
	first, err := m.OpenSession(ctx, at, "", work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := envOf(t, ctx, first, EnvPiAgentDir); got != full {
		t.Fatalf("write session runs in %s", got)
	}
	same, err := m.OpenSession(ctx, at, first.ID(), work, nil)
	if err != nil || same.ID() != first.ID() || same.(ContextResetter).ContextReset() != "" {
		t.Fatalf("same-class resume = %v, %v", same, err)
	}
	// The owner narrows the policy at run time.
	if err := m.Set(Pi, Config{Command: adapter, Permission: "read", Env: env}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.Prompt(ctx, "after narrowing", nil); err == nil {
		t.Fatal("the full-home process kept serving after the policy narrowed")
	}
	fresh, err := m.OpenSession(ctx, at, first.ID(), work, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The mock numbers sessions per process, so the new process's first
	// session may reuse the ID; the reset and the process it runs in say
	// it is new.
	if fresh.(ContextResetter).ContextReset() != ContextResetPolicy {
		t.Fatalf("read-only open resumed the write session: %s (%q)", fresh.ID(), fresh.(ContextResetter).ContextReset())
	}
	if got := envOf(t, ctx, fresh, EnvPiAgentDir); got != readOnly {
		t.Fatalf("reset session runs in %s, want %s", got, readOnly)
	}
	if got := envOf(t, ctx, fresh, pihome.EnvCommand); got != filepath.Join(readOnly, "steve-pi") {
		t.Fatalf("reset session starts pi through %s", got)
	}
	brandNew, err := m.OpenSession(ctx, at, "", work, nil)
	if err != nil || envOf(t, ctx, brandNew, EnvPiAgentDir) != readOnly {
		t.Fatalf("new session after narrowing: %v", err)
	}
	if pihome.Resumable(env, fresh.ID(), pihome.Full) || !pihome.Resumable(env, fresh.ID(), pihome.ReadOnly) {
		t.Fatal("the reset session is not recorded under its process's class")
	}
	if got := startsOf(t, starts); len(got) != 2 || !strings.HasPrefix(got[0], full+" ") || got[1] != readOnly+" "+filepath.Join(readOnly, "steve-pi") {
		t.Fatalf("adapter starts = %q", got)
	}
	// Publishing a configuration that widens it again does the same.
	prepared, err := NewManager(map[string]Config{Pi: {Command: adapter, Permission: "auto", Env: env}})
	if err != nil {
		t.Fatal(err)
	}
	m.Publish(prepared)
	widened, err := m.OpenSession(ctx, at, "", work, nil)
	if err != nil || envOf(t, ctx, widened, EnvPiAgentDir) != full {
		t.Fatalf("session after widening: %v", err)
	}
	if got := startsOf(t, starts); len(got) != 3 {
		t.Fatalf("adapter starts after widening = %q", got)
	}
}

// When the launcher refuses a session file (kept in the other home), the
// open starts afresh instead of failing.
func TestPiRefusedResumeStartsFresh(t *testing.T) {
	adapter, _ := buildPiMock(t)
	base := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	env := []string{pihome.EnvHomes + "=" + base, "MOCKAGENT_REJECT_LOAD=1"}
	m, err := NewManager(map[string]Config{Pi: {Command: adapter, Permission: "read", Env: env}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// What steve-pi leaves when it refuses the file of session "old-1".
	note := filepath.Join(pihome.Dir(base, pihome.ReadOnly), "refused")
	if err := os.MkdirAll(note, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(note, "2026_old-1.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := m.OpenSession(ctx, Placement{Harness: Pi}, "old-1", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID() == "old-1" || r.(ContextResetter).ContextReset() != ContextResetPolicy {
		t.Fatalf("refused resume = %s (%q)", r.ID(), r.(ContextResetter).ContextReset())
	}
	// Another load failure stays a failure.
	if _, err := m.OpenSession(ctx, Placement{Harness: Pi}, "old-2", t.TempDir(), nil); err == nil {
		t.Fatal("an unrelated load failure started afresh")
	}
}
