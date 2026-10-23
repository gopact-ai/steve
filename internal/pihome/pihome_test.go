package pihome

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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

func TestLaunchChoosesTheHomeThePolicyNeeds(t *testing.T) {
	base := t.TempDir()
	if err := Prepare(base, ""); err != nil {
		t.Fatal(err)
	}
	start := []string{"OTHER=1", EnvHomes + "=" + base, EnvAgentDir + "=" + Dir(base, ReadOnly), EnvCommand + "=/opt/pi/bin/pi"}
	for policy, want := range map[string]string{"read": ReadOnly, "deny": ReadOnly, "": ReadOnly, "write": Full, "auto": Full, "always_allow": Full} {
		env, err := Launch(start, policy)
		if err != nil {
			t.Fatalf("%q: %v", policy, err)
		}
		if got := lookup(env, EnvAgentDir); got != Dir(base, want) {
			t.Fatalf("%q selected %s, want %s", policy, got, Dir(base, want))
		}
		// pi-acp starts the home's launcher, which runs the registered pi.
		if lookup(env, EnvCommand) != filepath.Join(Dir(base, want), wrapper) || lookup(env, EnvReal) != "/opt/pi/bin/pi" {
			t.Fatalf("%q launcher env = %v", policy, env)
		}
		if !slices.Contains(env, "OTHER=1") {
			t.Fatalf("%q dropped the rest of the environment: %v", policy, env)
		}
		// Launching again from a launched env keeps the real pi.
		again, err := Launch(env, policy)
		if err != nil || lookup(again, EnvReal) != "/opt/pi/bin/pi" {
			t.Fatalf("%q relaunch = %v, %v", policy, again, err)
		}
	}
}

func TestLaunchIgnoresAnOperatorAgentDir(t *testing.T) {
	base := t.TempDir()
	// An operator's own PI_CODING_AGENT_DIR after Steve's never wins.
	env := []string{EnvHomes + "=" + base, EnvAgentDir + "=/home/op/.pi/agent"}
	for policy, want := range map[string]string{"read": ReadOnly, "write": Full} {
		got, err := Launch(env, policy)
		if err != nil || lookup(got, EnvAgentDir) != Dir(base, want) {
			t.Fatalf("%q = %v, %v", policy, got, err)
		}
	}
	// Without Steve's homes no policy is started, a write one included.
	own := []string{EnvAgentDir + "=" + t.TempDir()}
	for _, policy := range []string{"read", "write"} {
		if _, err := Launch(own, policy); !errors.Is(err, ErrUnenforced) {
			t.Fatalf("%q without Steve's homes: %v", policy, err)
		}
	}
	if _, err := Launch(nil, "read"); !errors.Is(err, ErrUnenforced) {
		t.Fatalf("no env: %v", err)
	}
}

func TestEnforcedChecksContentNotJustTheMarker(t *testing.T) {
	for name, tamper := range map[string]func(dir string) error{
		"extension": func(dir string) error {
			if err := os.MkdirAll(filepath.Join(dir, "extensions"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "extensions", "x.js"), []byte("export default () => {}"), 0o600)
		},
		"tools": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"defaultTools":["read","bash"],"defaultProjectTrust":"never"}`), 0o600)
		},
		"trust": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"defaultTools":["read","grep","find","ls"],"defaultProjectTrust":"always"}`), 0o600)
		},
		"launcher": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, wrapper), []byte("#!/bin/sh\nexec pi \"$@\"\n"), 0o700)
		},
	} {
		base := t.TempDir()
		if err := Prepare(base, ""); err != nil {
			t.Fatal(err)
		}
		dir := Dir(base, ReadOnly)
		if err := tamper(dir); err != nil {
			t.Fatal(err)
		}
		if Enforced(dir) != "" {
			t.Fatalf("%s: tampered home still reported enforced", name)
		}
		// Every start prepares the home again before it is used.
		env, err := Launch([]string{EnvHomes + "=" + base}, "read")
		if err != nil || Enforced(dir) != ReadOnly {
			t.Fatalf("%s: launch did not repair the home: %v", name, err)
		}
		if entries, _ := os.ReadDir(filepath.Join(dir, "extensions")); len(entries) != 0 || lookup(env, EnvAgentDir) != dir {
			t.Fatalf("%s: repaired home = %v, env %v", name, entries, env)
		}
	}
}

func TestLaunchKeepsTheOperatorSource(t *testing.T) {
	src := operatorPi(t)
	base := t.TempDir()
	if err := Prepare(base, src); err != nil {
		t.Fatal(err)
	}
	if _, err := Launch([]string{EnvHomes + "=" + base}, "write"); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(Dir(base, Full), "auth.json")); err != nil || target != filepath.Join(src, "auth.json") {
		t.Fatalf("auth link after relaunch = %q, %v", target, err)
	}
}

func TestResumeUnderAnotherClassStartsFresh(t *testing.T) {
	base := t.TempDir()
	env := []string{EnvHomes + "=" + base}
	if !Resumable(env, "s-unknown", "read") {
		t.Fatal("a session Steve has no record of must stay resumable")
	}
	if err := Remember(env, "s1", "write"); err != nil {
		t.Fatal(err)
	}
	if !Resumable(env, "s1", "auto") {
		t.Fatal("same class refused")
	}
	// The owner narrowed the policy: resuming would restore bash/edit/write.
	if Resumable(env, "s1", "read") || Resumable(env, "s1", "deny") {
		t.Fatal("write-class session resumable under read")
	}
	if err := Remember(env, "s2", "read"); err != nil {
		t.Fatal(err)
	}
	if Resumable(env, "s2", "write") || !Resumable(env, "s2", "") {
		t.Fatal("read-class session class mismatch not seen")
	}
}

// The launcher, run with a fake pi, appends the read-only allowlist and
// refuses a session file kept in the other home.
func TestWrapperPinsToolsAndRefusesTheOtherHome(t *testing.T) {
	base := t.TempDir()
	if err := Prepare(base, ""); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(class string, args ...string) (string, error) {
		env, err := Launch([]string{EnvHomes + "=" + base, EnvCommand + "=" + fake}, map[string]string{ReadOnly: "read", Full: "write"}[class])
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(lookup(env, EnvCommand), args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	ro := Dir(base, ReadOnly)
	out, err := run(ReadOnly, "--mode", "rpc", "--session", filepath.Join(ro, "sessions", "a.jsonl"))
	if err != nil || out != "--mode rpc --session "+filepath.Join(ro, "sessions", "a.jsonl")+" --no-extensions --tools read,grep,find,ls" {
		t.Fatalf("read-only launch = %q, %v", out, err)
	}
	out, err = run(Full, "--mode", "rpc")
	if err != nil || out != "--mode rpc --no-extensions" {
		t.Fatalf("full launch = %q, %v", out, err)
	}
	// A session file from the full home under the read-only one.
	out, err = run(ReadOnly, "--mode", "rpc", "--session", filepath.Join(Dir(base, Full), "sessions", "b.jsonl"))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(out, "another permission policy") {
		t.Fatalf("cross-home resume = %q, %v", out, err)
	}
}
