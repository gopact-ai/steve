package pihome

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// operatorPi is a ~/.pi/agent with model access, credentials and the
// terminal-only settings Steve must not inherit.
func operatorPi(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	settings := `{"defaultProvider":"relay","defaultModel":"relay/m1","defaultThinkingLevel":"high","enabledModels":["relay/*"],
"extensions":["/home/op/ext.ts"],"packages":["npm:pkg"],"skills":["/home/op/skills"],"theme":"dark","defaultTools":["read","bash","edit","write","+codemode"],"defaultProjectTrust":"always"}`
	for name, body := range map[string]string{"settings.json": settings, "auth.json": `{"relay":{"type":"api_key","key":"secret-auth"}}`, "models.json": `{"providers":{"relay":{"apiKey":"secret-model-key"}}}`} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func settingsOf(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPrepareKeepsModelAccessAndDropsTerminalSettings(t *testing.T) {
	src, base := operatorPi(t), t.TempDir()
	// Every start prepares again; the second pass replaces the first.
	for range 2 {
		if err := Prepare(base, src); err != nil {
			t.Fatal(err)
		}
	}
	for _, class := range []string{Full, ReadOnly} {
		dir := Dir(base, class)
		got := settingsOf(t, dir)
		if got["defaultModel"] != "relay/m1" || got["defaultProvider"] != "relay" || got["defaultThinkingLevel"] != "high" {
			t.Fatalf("%s lost model access: %v", class, got)
		}
		for _, key := range []string{"extensions", "packages", "skills", "theme"} {
			if _, kept := got[key]; kept {
				t.Fatalf("%s kept the operator's %s: %v", class, key, got)
			}
		}
		if got["defaultProjectTrust"] != "never" {
			t.Fatalf("%s lets a working folder load its own settings: %v", class, got)
		}
		// Credentials and provider keys are links to the operator's files,
		// never copies: a second copy of a secret is never written.
		for _, name := range []string{"auth.json", "models.json"} {
			info, err := os.Lstat(filepath.Join(dir, name))
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s/%s is not a link: %v", class, name, err)
			}
			if target, _ := os.Readlink(filepath.Join(dir, name)); target != filepath.Join(src, name) {
				t.Fatalf("%s/%s links to %s", class, name, target)
			}
		}
		if Enforced(dir) != class {
			t.Fatalf("%s home records %q", class, Enforced(dir))
		}
		for _, name := range []string{"settings.json", marker} {
			raw, _ := os.ReadFile(filepath.Join(dir, name))
			if strings.Contains(string(raw), "secret") {
				t.Fatalf("%s/%s contains a secret: %s", class, name, raw)
			}
		}
	}
	// Both homes see the one skills directory Steve fills.
	if err := os.MkdirAll(filepath.Join(base, "skills", "one"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{Full, ReadOnly} {
		if _, err := os.Stat(filepath.Join(Dir(base, class), "skills", "one")); err != nil {
			t.Fatalf("%s home does not see shared skills: %v", class, err)
		}
	}
}

func TestReadOnlyHomeLoadsNoWriteOrShellTools(t *testing.T) {
	base := t.TempDir()
	if err := Prepare(base, operatorPi(t)); err != nil {
		t.Fatal(err)
	}
	tools, _ := settingsOf(t, Dir(base, ReadOnly))["defaultTools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(string))
	}
	if !slices.Equal(names, ReadOnlyTools) {
		t.Fatalf("read-only tools = %v", names)
	}
	for _, banned := range []string{"bash", "edit", "write", "powershell", "codemode"} {
		if slices.Contains(names, banned) {
			t.Fatalf("read-only home loads %s", banned)
		}
	}
	// The full home keeps pi's own defaults: the operator's list (with its
	// extension tool) is not carried over either.
	if _, set := settingsOf(t, Dir(base, Full))["defaultTools"]; set {
		t.Fatal("full home pinned a tool list")
	}
}

func TestPrepareWithoutOperatorPi(t *testing.T) {
	base := t.TempDir()
	if err := Prepare(base, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(Dir(base, Full), "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("auth link without an operator pi: %v", err)
	}
	if Enforced(Dir(base, ReadOnly)) != ReadOnly {
		t.Fatal("read-only home unmarked")
	}
}

func TestSelectChoosesTheHomeThePolicyNeeds(t *testing.T) {
	base := t.TempDir()
	if err := Prepare(base, ""); err != nil {
		t.Fatal(err)
	}
	start := []string{"OTHER=1", EnvHomes + "=" + base, EnvAgentDir + "=" + Dir(base, ReadOnly)}
	for policy, want := range map[string]string{"read": ReadOnly, "deny": ReadOnly, "": ReadOnly, "write": Full, "auto": Full, "always_allow": Full} {
		env, err := Select(start, policy)
		if err != nil {
			t.Fatalf("%q: %v", policy, err)
		}
		if got := lookup(env, EnvAgentDir); got != Dir(base, want) {
			t.Fatalf("%q selected %s, want %s", policy, got, Dir(base, want))
		}
		if !slices.Contains(env, "OTHER=1") {
			t.Fatalf("%q dropped the rest of the environment: %v", policy, env)
		}
	}
}

func TestSelectRefusesReadOnlyWhereItIsNotEnforced(t *testing.T) {
	own := t.TempDir() // an operator's PI_CODING_AGENT_DIR, not prepared by Steve
	env := []string{EnvAgentDir + "=" + own}
	if _, err := Select(env, "read"); !errors.Is(err, ErrUnenforced) {
		t.Fatalf("read-only policy on an unenforced home: %v", err)
	}
	if got, err := Select(env, "write"); err != nil || !slices.Equal(got, env) {
		t.Fatalf("write policy = %v, %v", got, err)
	}
	// A home Steve prepared read-only is accepted as is.
	if err := PrepareHome(own, "", ReadOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(env, "read"); err != nil {
		t.Fatal(err)
	}
	// The default ~/.pi/agent is refused too.
	t.Setenv("HOME", t.TempDir())
	if _, err := Select(nil, "read"); !errors.Is(err, ErrUnenforced) {
		t.Fatalf("default pi home under read-only: %v", err)
	}
}
