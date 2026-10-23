package pihome

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	t.Setenv("HOME", t.TempDir())
	env := []string{EnvHomes + "=" + base}
	if !Resumable(env, "s-unknown", ReadOnly) {
		t.Fatal("a session Steve has no record of must stay resumable")
	}
	if err := Remember(env, "s1", Full); err != nil {
		t.Fatal(err)
	}
	if !Resumable(env, "s1", Full) {
		t.Fatal("same class refused")
	}
	// The owner narrowed the policy: resuming would restore bash/edit/write.
	if Resumable(env, "s1", ReadOnly) {
		t.Fatal("full-class session resumable under read-only")
	}
	if err := Remember(env, "s2", ReadOnly); err != nil {
		t.Fatal(err)
	}
	if Resumable(env, "s2", Full) || !Resumable(env, "s2", ReadOnly) {
		t.Fatal("read-only session class mismatch not seen")
	}
	// The record is replaced by rename and is never a link or half file.
	if info, err := os.Lstat(filepath.Join(base, classes)); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("classes.json = %v, %v", info, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(base, tmpName+"*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

// A record that does not parse is moved aside, not overwritten.
func TestCorruptClassRecordIsQuarantined(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	env := []string{EnvHomes + "=" + base}
	if err := os.WriteFile(filepath.Join(base, classes), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remember(env, "s1", Full); err != nil {
		t.Fatal(err)
	}
	aside, _ := filepath.Glob(filepath.Join(base, classes+".corrupt-*"))
	if len(aside) != 1 {
		t.Fatalf("corrupt record not kept aside: %v", aside)
	}
	if raw, _ := os.ReadFile(aside[0]); string(raw) != "{not json" {
		t.Fatalf("quarantined record changed: %q", raw)
	}
	if Resumable(env, "s1", ReadOnly) {
		t.Fatal("record written after quarantine not used")
	}
}

// Remember from many goroutines keeps every record (one lock, whole-file
// rename).
func TestRememberKeepsConcurrentRecords(t *testing.T) {
	base := t.TempDir()
	env := []string{EnvHomes + "=" + base}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Remember(env, fmt.Sprintf("s%d", i), Full); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	unlock, _ := lockBase(base)
	defer unlock()
	if got := len(readClasses(base).Sessions); got != 32 {
		t.Fatalf("records = %d, want 32", got)
	}
}

// pi-acp's map places a session's file; one outside this class's home is
// not resumed even without Steve's record.
func TestResumableFollowsTheSessionMap(t *testing.T) {
	base := t.TempDir()
	home := t.TempDir()
	if err := Prepare(base, ""); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{Full, ReadOnly} {
		if err := os.MkdirAll(filepath.Join(Dir(base, class), "sessions", "--w--"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	full := filepath.Join(Dir(base, Full), "sessions", "--w--", "x_s9.jsonl")
	escape := filepath.Join(Dir(base, ReadOnly), "sessions", "..", "..", "full", "sessions", "--w--", "x_s8.jsonl")
	own := filepath.Join(Dir(base, ReadOnly), "sessions", "--w--", "x_s7.jsonl")
	mapped := fmt.Sprintf(`{"version":1,"sessions":{"s9":{"sessionFile":%q},"s8":{"sessionFile":%q},"s7":{"sessionFile":%q}}}`, full, escape, own)
	if err := os.MkdirAll(filepath.Join(home, ".pi", "pi-acp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".pi", "pi-acp", "session-map.json"), []byte(mapped), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{EnvHomes + "=" + base, "HOME=" + home}
	if Resumable(env, "s9", ReadOnly) || Resumable(env, "s8", ReadOnly) || !Resumable(env, "s7", ReadOnly) || !Resumable(env, "s9", Full) {
		t.Fatal("session map placement not followed")
	}
}

// The launcher, run with a fake pi, appends the read-only allowlist and
// refuses, with a note for Steve, a session file that does not resolve
// into its home.
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
	ro, full := Dir(base, ReadOnly), Dir(base, Full)
	for _, dir := range []string{ro, full} {
		if err := os.MkdirAll(filepath.Join(dir, "sessions", "--w--"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	own := filepath.Join(ro, "sessions", "--w--", "a.jsonl")
	out, err := run(ReadOnly, "--mode", "rpc", "--session", own)
	if err != nil || out != "--mode rpc --session "+own+" --no-extensions --tools read,grep,find,ls" {
		t.Fatalf("read-only launch = %q, %v", out, err)
	}
	out, err = run(Full, "--mode", "rpc")
	if err != nil || out != "--mode rpc --no-extensions" {
		t.Fatalf("full launch = %q, %v", out, err)
	}
	link := filepath.Join(ro, "sessions", "--w--", "linked")
	if err := os.Symlink(filepath.Join(full, "sessions", "--w--"), link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(full, "sessions", "--w--", "f.jsonl"), filepath.Join(ro, "sessions", "--w--", "g_s5.jsonl")); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"other home":    filepath.Join(full, "sessions", "--w--", "b_s1.jsonl"),
		"dot-dot":       filepath.Join(ro, "sessions", "..", "..", "full", "sessions", "--w--", "c_s2.jsonl"),
		"linked dir":    filepath.Join(link, "d_s3.jsonl"),
		"relative":      "sessions/--w--/e_s4.jsonl",
		"linked file":   filepath.Join(ro, "sessions", "--w--", "g_s5.jsonl"),
		"sessions peer": filepath.Join(ro, "sessions-x", "h_s6.jsonl"),
	} {
		out, err = run(ReadOnly, "--mode", "rpc", "--session", path)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(out, "another policy") {
			t.Fatalf("%s: resume = %q, %v", name, out, err)
		}
	}
	env := []string{EnvHomes + "=" + base}
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		if !Refused(env, id, ReadOnly) {
			t.Fatalf("no refusal note for %s", id)
		}
		if Refused(env, id, ReadOnly) {
			t.Fatalf("refusal note for %s not cleared", id)
		}
	}
	if Refused(env, "a", Full) {
		t.Fatal("refusal reported for the wrong class")
	}
}

// pi's configuration files Steve does not prepare are removed by Launch and
// make Enforced fail.
func TestLaunchRemovesConfigurationPiWouldLoad(t *testing.T) {
	base := t.TempDir()
	if err := Prepare(base, ""); err != nil {
		t.Fatal(err)
	}
	dir := Dir(base, ReadOnly)
	for _, name := range []string{"mcp.json", "trust.json", "SYSTEM.md", "APPEND_SYSTEM.md", "AGENTS.md", "keybindings.json", "mcp-auth.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"mcpServers":{"x":{"command":"touch"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if Enforced(dir) != "" {
			t.Fatalf("%s: home still enforced", name)
		}
		if _, err := Launch([]string{EnvHomes + "=" + base}, "read"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived Launch: %v", name, err)
		}
	}
	for _, name := range []string{"tools", "themes", "prompts", "npm", "git"} {
		if err := os.MkdirAll(filepath.Join(dir, name, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if Enforced(dir) != "" {
		t.Fatal("home with tools/themes/prompts/npm/git still enforced")
	}
	// pi's own run-time state stays.
	for _, name := range []string{"sessions", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"models-store.json", "crashes.json", "settings.json.lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Launch([]string{EnvHomes + "=" + base}, "read"); err != nil || Enforced(dir) != ReadOnly {
		t.Fatalf("launch = %v, enforced %q", err, Enforced(dir))
	}
	for _, name := range []string{"sessions", "tmp", "models-store.json", "crashes.json", "settings.json.lock"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s removed: %v", name, err)
		}
	}
}

// A launch while another pi starts from the same home never leaves the
// launcher or settings missing or partial: an unchanged home is left
// alone, a changed one is replaced by rename.
func TestLaunchNeverLeavesTheHomeHalfWritten(t *testing.T) {
	base := t.TempDir()
	if err := Prepare(base, ""); err != nil {
		t.Fatal(err)
	}
	dir := Dir(base, ReadOnly)
	launcher, settings := filepath.Join(dir, wrapper), filepath.Join(dir, "settings.json")
	before, _ := os.Stat(launcher)
	if _, err := Launch([]string{EnvHomes + "=" + base}, "read"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(launcher); !os.SameFile(before, after) {
		t.Fatal("an unchanged launcher was rewritten")
	}
	want, _ := os.ReadFile(launcher)
	wantSettings, _ := os.ReadFile(settings)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Another process changes the home; the launch repairs it.
			_ = os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{}"), 0o600)
			if _, err := Launch([]string{EnvHomes + "=" + base}, "read"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for range 2000 {
		got, err := os.ReadFile(launcher)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("launcher read during launch: %v (%d bytes)", err, len(got))
		}
		got, err = os.ReadFile(settings)
		if err != nil || !bytes.Equal(got, wantSettings) {
			t.Fatalf("settings read during launch: %v (%d bytes)", err, len(got))
		}
	}
	close(stop)
	wg.Wait()
}
