package skills

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestAddDestsMaterializesSelectedRuntimeWithoutRestartingAgents(t *testing.T) {
	root := t.TempDir()
	search := filepath.Join(root, "catalog")
	writeSkill(t, search, "remind", "remind")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	var restarts int
	live := &Live{Map: m, After: func() error { restarts++; return nil }}
	dest := filepath.Join(root, "selected-runtime")
	if err := live.AddDests(dest, dest); err != nil {
		t.Fatal(err)
	}
	if len(live.Dests) != 1 || live.Dests[0] != dest || restarts != 0 {
		t.Fatalf("runtime registration: dests=%v restarts=%d", live.Dests, restarts)
	}
	if _, err := os.Readlink(filepath.Join(dest, "remind")); err != nil {
		t.Fatal(err)
	}
	if err := live.Disable("remind"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatal("later skill change did not retain normal restart behavior")
	}
	if _, err := os.Lstat(filepath.Join(dest, "remind")); !os.IsNotExist(err) {
		t.Fatal("later skill change did not reach newly registered runtime")
	}
}

func TestLiveApplyOnChangeOnly(t *testing.T) {
	root := t.TempDir()
	search := filepath.Join(root, "catalog")
	writeSkill(t, search, "remind", "remind")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "runtime")
	var restarts int
	live := &Live{
		Map:   m,
		Dests: []string{dest},
		After: func() error {
			restarts++
			return nil
		},
	}
	if err := live.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatalf("restarts = %d, want 1", restarts)
	}
	if _, err := os.Readlink(filepath.Join(dest, "remind")); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("remind"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatalf("idempotent enable restarted: %d", restarts)
	}
	if err := live.Disable("remind"); err != nil {
		t.Fatal(err)
	}
	if restarts != 2 {
		t.Fatalf("restarts after disable = %d, want 2", restarts)
	}
	if _, err := os.Stat(filepath.Join(dest, "remind")); !os.IsNotExist(err) {
		t.Fatal("disabled skill still linked")
	}
}

func TestLiveAddPathDoesNotRestart(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, extra, "calendar", "cal")
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	var restarts int
	live := &Live{
		Map:   m,
		Dests: []string{filepath.Join(root, "runtime")},
		After: func() error {
			restarts++
			return nil
		},
	}
	if err := live.AddPath(extra); err != nil {
		t.Fatal(err)
	}
	if restarts != 0 {
		t.Fatalf("add path restarted: %d", restarts)
	}
	if err := live.Enable("calendar"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatalf("enable after path add restarts = %d", restarts)
	}
}

func TestLiveUpdateSourcesContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// All fetches stay in temporary repositories, without user git config.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "--template=", "-b", "main")
	writeSkill(t, repo, "alpha", "enabled")
	writeSkill(t, repo, "beta", "disabled")
	run("add", ".")
	run("commit", "-qm", "initial fixture")

	live, dest := newTestLive(t)
	src, err := live.AddSource(context.Background(), "file://"+filepath.ToSlash(repo))
	if err != nil {
		t.Fatal(err)
	}
	restarts := 0
	live.After = func() error { restarts++; return nil }
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatalf("enable restarts = %d, want 1", restarts)
	}
	update := func(t *testing.T, wantRestarts int) {
		t.Helper()
		out, err := live.UpdateSources(context.Background())
		if err != nil || len(out) != 1 || out[0].Error != "" {
			t.Fatalf("update = %+v, err = %v", out, err)
		}
		if restarts != wantRestarts {
			t.Fatalf("restarts = %d, want %d", restarts, wantRestarts)
		}
	}
	t.Run("no-op", func(t *testing.T) { update(t, 1) })
	t.Run("disabled content", func(t *testing.T) {
		writeSkill(t, repo, "beta", "disabled changed")
		run("add", ".")
		run("commit", "-qm", "change disabled fixture")
		update(t, 1)
	})
	t.Run("enabled content through source symlink", func(t *testing.T) {
		writeSkill(t, repo, "alpha", "enabled changed")
		run("add", ".")
		run("commit", "-qm", "change enabled fixture")
		update(t, 2)
		body, err := os.ReadFile(filepath.Join(dest, "alpha", "SKILL.md"))
		if err != nil || string(body) != "enabled changed" {
			t.Fatalf("materialized content = %q, err = %v", body, err)
		}
	})
	t.Run("source removal falls back to same name", func(t *testing.T) {
		fallback := t.TempDir()
		target := writeSkill(t, fallback, "alpha", "fallback")
		if err := live.AddPath(fallback); err != nil {
			t.Fatal(err)
		}
		if err := live.RemoveSource(src.Slug); err != nil {
			t.Fatal(err)
		}
		assertLiveLink(t, dest, "alpha", target)
		if restarts != 3 {
			t.Fatalf("source fallback restarts = %d, want 3", restarts)
		}
	})
}

func TestLiveRemovePathSameNameSource(t *testing.T) {
	for _, body := range []string{"original", "changed"} {
		t.Run(body, func(t *testing.T) {
			live, dest := newTestLive(t)
			first, second := t.TempDir(), t.TempDir()
			writeSkill(t, first, "alpha", "original")
			target := writeSkill(t, second, "alpha", body)
			for _, path := range []string{first, second} {
				if err := live.AddPath(path); err != nil {
					t.Fatal(err)
				}
			}
			restarts := 0
			live.After = func() error { restarts++; return nil }
			if err := live.Enable("alpha"); err != nil {
				t.Fatal(err)
			}
			if err := live.RemovePath(first); err != nil {
				t.Fatal(err)
			}
			assertLiveLink(t, dest, "alpha", target)
			want := 1
			if body != "original" {
				want = 2
			}
			if restarts != want {
				t.Fatalf("restarts = %d, want %d", restarts, want)
			}
		})
	}
}

func TestLiveAfterFailureRetriesSameValue(t *testing.T) {
	for _, operation := range []string{"enable", "disable", "apply", "update"} {
		t.Run(operation, func(t *testing.T) {
			live, _ := newTestLive(t)
			search := t.TempDir()
			writeSkill(t, search, "alpha", "original")
			if err := live.AddPath(search); err != nil {
				t.Fatal(err)
			}
			if operation != "enable" {
				if err := live.Map.Enable("alpha"); err != nil {
					t.Fatal(err)
				}
			}
			if err := live.Apply(); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("propagation failed")
			calls := 0
			live.After = func() error {
				calls++
				if calls == 1 {
					return failure
				}
				return nil
			}
			var op func() error
			switch operation {
			case "enable":
				op = func() error { return live.Enable("alpha") }
			case "disable":
				op = func() error { return live.Disable("alpha") }
			case "apply":
				writeSkill(t, search, "alpha", "changed")
				op = live.Apply
			case "update":
				writeSkill(t, search, "alpha", "changed")
				op = func() error {
					_, err := live.UpdateSources(context.Background())
					return err
				}
			}
			if err := op(); !errors.Is(err, failure) || !strings.HasPrefix(err.Error(), "apply skills: ") {
				t.Fatalf("first apply error = %v, want %v", err, failure)
			}
			for range 2 {
				if err := op(); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 2 {
				t.Fatalf("After calls = %d, want failed attempt + successful retry", calls)
			}
		})
	}
}

func TestLiveInitialApplyPreparesBeforeAfterIsWired(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "enabled"}[enabled], func(t *testing.T) {
			live, dest := newTestLive(t)
			search := t.TempDir()
			target := writeSkill(t, search, "alpha", "original")
			if enabled {
				if err := live.Map.Enable(target); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(dest, 0o700); err != nil {
				t.Fatal(err)
			}
			stale := filepath.Join(dest, "stale")
			if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := live.Apply(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(stale); !os.IsNotExist(err) {
				t.Fatalf("initial preparation did not remove stale entry: %v", err)
			}
			if enabled {
				assertLiveLink(t, dest, "alpha", target)
			}
			live.After = func() error { t.Error("unchanged boot state restarted"); return nil }
			if err := live.Apply(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLiveRealSourceChangePreparesWithoutRestart(t *testing.T) {
	live, dest := newTestLive(t)
	search, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, first, "alpha", "identical")
	target := writeSkill(t, second, "alpha", "identical")
	sourceLink := filepath.Join(search, "alpha")
	if err := os.Symlink(filepath.Join(first, "alpha"), sourceLink); err != nil {
		t.Fatal(err)
	}
	if err := live.AddPath(search); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	live.After = func() error { t.Error("same content restarted"); return nil }
	marker := filepath.Join(dest, "preparation-marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("no-op unexpectedly rematerialized: %v", err)
	}
	if err := os.Remove(sourceLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, sourceLink); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("real source change did not rematerialize: %v", err)
	}
	assertLiveLink(t, dest, "alpha", sourceLink)
}

func TestLiveSameNameContentChange(t *testing.T) {
	live, _ := newTestLive(t)
	search := t.TempDir()
	target := writeSkill(t, search, "alpha", "original")
	if err := live.AddPath(search); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	live.After = func() error { calls++; return nil }
	resource := filepath.Join(target, "helper.sh")
	if err := os.WriteFile(resource, []byte("echo hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("added resource restarts = %d, want 1", calls)
	}
	if err := os.Chmod(resource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("executable bit change restarts = %d, want 2", calls)
	}
	if err := os.Chmod(resource, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("irrelevant mode change restarted: %d", calls)
	}
}

func TestLiveFailedChangeThenRevertStillApplies(t *testing.T) {
	live, dest := newTestLive(t)
	target := writeSkill(t, t.TempDir(), "alpha", "original")
	if err := live.Enable(target); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("partially restarted")
	live.After = func() error { return failure }
	if err := live.Disable("alpha"); !errors.Is(err, failure) {
		t.Fatalf("disable = %v, want %v", err, failure)
	}
	calls := 0
	live.After = func() error { calls++; return nil }
	if err := live.Enable(target); err != nil {
		t.Fatal(err)
	}
	assertLiveLink(t, dest, "alpha", target)
	if calls != 1 {
		t.Fatalf("revert must reconcile partially applied state: calls = %d", calls)
	}
}

func TestLiveMaterializeFailureRetriesSameValue(t *testing.T) {
	live, dest := newTestLive(t)
	target := writeSkill(t, t.TempDir(), "alpha", "original")
	if err := os.WriteFile(dest, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	live.After = func() error { calls++; return nil }
	if err := live.Enable(target); err == nil {
		t.Fatal("materialize unexpectedly succeeded over a file")
	}
	if calls != 0 {
		t.Fatal("After ran before preparation succeeded")
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if err := live.Enable(target); err != nil {
		t.Fatal(err)
	}
	assertLiveLink(t, dest, "alpha", target)
	if calls != 1 {
		t.Fatalf("retry calls = %d, want 1", calls)
	}
}

func TestLiveAddDestsPreparesWithAppliedBaseline(t *testing.T) {
	live, first := newTestLive(t)
	target := writeSkill(t, t.TempDir(), "alpha", "original")
	if err := live.Enable(target); err != nil {
		t.Fatal(err)
	}
	calls := 0
	live.After = func() error { calls++; return nil }
	second := filepath.Join(t.TempDir(), "new-runtime")
	if err := live.AddDests(second, second, first); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []string{first, second} {
		assertLiveLink(t, dest, "alpha", target)
	}
	if err := live.Apply(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(live.Dests) != 2 {
		t.Fatalf("registration: calls = %d, dests = %v", calls, live.Dests)
	}
	if err := live.Disable("alpha"); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []string{first, second} {
		if _, err := os.Lstat(filepath.Join(dest, "alpha")); !os.IsNotExist(err) {
			t.Fatalf("disabled skill remains at %s: %v", dest, err)
		}
	}
	if calls != 1 {
		t.Fatalf("change after registration: calls = %d, want 1", calls)
	}
}

func TestLivePackErrorDoesNotPrepareOrMarkApplied(t *testing.T) {
	live, dest := newTestLive(t)
	// Map accepts this absolute skill path, but Pack rejects hidden names.
	target := writeSkill(t, t.TempDir(), ".alpha", "original")
	calls := 0
	live.After = func() error { calls++; return nil }
	for range 2 {
		if err := live.Enable(target); err == nil || !strings.Contains(err.Error(), "pack enabled skills:") {
			t.Fatalf("pack error was lost: %v", err)
		}
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) || calls != 0 {
		t.Fatalf("pack failure caused side effects: stat = %v, calls = %d", err, calls)
	}
	if err := live.Disable(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); err != nil || calls != 1 {
		t.Fatalf("recovery did not apply: stat = %v, calls = %d", err, calls)
	}
}

func TestLiveRemoveSourceRetriesCommittedDeletion(t *testing.T) {
	for _, retry := range []string{"same-slug", "apply", "other-change", "reinstalled", "map-error"} {
		t.Run(retry, func(t *testing.T) {
			live, dest := newTestLive(t)
			clone := t.TempDir()
			// RemoveSource only needs the persisted installed-source record.
			// Its filesystem effects are real, entirely inside temporary dirs.
			src := Source{Slug: "fixture", Dir: clone, Root: filepath.Join(t.TempDir(), "source.skills")}
			install := func() {
				t.Helper()
				writeSkill(t, clone, "alpha", "original")
				if err := os.MkdirAll(src.Root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(clone, "alpha"), filepath.Join(src.Root, "alpha")); err != nil {
					t.Fatal(err)
				}
				if err := live.Map.writeLocked(fileData{SearchPaths: []string{src.Root}, Enabled: []string{"alpha"}, Sources: []Source{src}}); err != nil {
					t.Fatal(err)
				}
			}
			install()
			if err := live.Apply(); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("remote still pending")
			calls := 0
			live.After = func() error { calls++; return failure }
			if err := live.RemoveSource(src.Slug); !errors.Is(err, failure) {
				t.Fatalf("first delete = %v", err)
			}
			if len(live.Map.Sources()) != 0 {
				t.Fatal("deletion was not committed")
			}
			for _, path := range []string{src.Dir, src.Root, filepath.Join(dest, "alpha")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("deleted path remains: %s: %v", path, err)
				}
			}
			if err := live.RemoveSource("unknown"); err == nil || calls != 1 {
				t.Fatalf("unknown slug was swallowed/reconciled: err=%v calls=%d", err, calls)
			}
			if err := live.RemoveSource(src.Slug); !errors.Is(err, failure) || calls != 2 {
				t.Fatalf("same deletion did not retry pending apply: err=%v calls=%d", err, calls)
			}
			switch retry {
			case "reinstalled":
				install()
			case "map-error":
				data, err := os.ReadFile(live.Map.path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(live.Map.path, []byte("{invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := live.RemoveSource(src.Slug); err == nil || !strings.Contains(err.Error(), "parse skills map") || calls != 2 {
					t.Fatalf("pending marker hid map error: err=%v calls=%d", err, calls)
				}
				if err := os.WriteFile(live.Map.path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			live.After = func() error { calls++; return nil }
			switch retry {
			case "same-slug", "reinstalled", "map-error":
				err := live.RemoveSource(src.Slug)
				if err != nil {
					t.Fatal(err)
				}
			case "apply":
				if err := live.Apply(); err != nil {
					t.Fatal(err)
				}
			case "other-change":
				if err := live.Disable("alpha"); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 3 {
				t.Fatalf("successful reconcile calls = %d, want 3", calls)
			}
			if len(live.Map.Sources()) != 0 {
				t.Fatal("reinstalled source was not removed")
			}
			if err := live.RemoveSource(src.Slug); err == nil || calls != 3 {
				t.Fatalf("completed deletion kept retry authority: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestLiveRemoveUnusedSourceDoesNotLeaveRetryMarker(t *testing.T) {
	live, _ := newTestLive(t)
	src := Source{Slug: "unused", Dir: t.TempDir(), Root: t.TempDir()}
	if err := live.Map.writeLocked(fileData{Sources: []Source{src}}); err != nil {
		t.Fatal(err)
	}
	if err := live.Apply(); err != nil {
		t.Fatal(err)
	}
	live.After = func() error { t.Error("unused source removal restarted"); return nil }
	if err := live.RemoveSource(src.Slug); err != nil {
		t.Fatal(err)
	}
	if err := live.RemoveSource(src.Slug); err == nil {
		t.Fatal("successful no-op application left a deletion retry marker")
	}
}

func TestLiveRemoveSourceRetriesAfterCommittedSyncFailure(t *testing.T) {
	t.Parallel()
	live, dest := newTestLive(t)
	clone := t.TempDir()
	writeSkill(t, clone, "alpha", "original")
	src := Source{Slug: "fixture", Dir: clone, Root: t.TempDir()}
	if err := live.Map.writeLocked(fileData{SearchPaths: []string{clone}, Enabled: []string{"alpha"}, Sources: []Source{src}}); err != nil {
		t.Fatal(err)
	}
	if err := live.Apply(); err != nil {
		t.Fatal(err)
	}
	failure := &os.PathError{Op: "sync", Path: filepath.Dir(live.Map.path), Err: syscall.EIO}
	syncs := 0
	live.Map.syncDir = func(string) error {
		syncs++
		reader, err := Open(live.Map.path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := reader.readLocked()
		if err != nil || len(got.Sources) != 0 || len(got.Enabled) != 0 {
			t.Fatalf("deletion is not visible at the sync fault: %+v, %v", got, err)
		}
		return failure
	}
	calls := 0
	afterFailure := errors.New("remote pending")
	live.After = func() error { calls++; return afterFailure }
	err := live.RemoveSource(src.Slug)
	if !errors.Is(err, failure) || syncs != 1 || calls != 0 {
		t.Fatalf("first delete lost its persistence error: err=%v syncs=%d calls=%d", err, syncs, calls)
	}
	if !live.pendingRemovals[src.Slug] || live.applied.hash != "" {
		t.Fatal("committed sync failure did not retain pending/invalidate applied")
	}
	if err := live.RemoveSource("unknown"); err == nil || calls != 0 {
		t.Fatalf("unknown slug gained retry authority: err=%v calls=%d", err, calls)
	}
	live.Map.syncDir = nil
	if err := live.RemoveSource(src.Slug); !errors.Is(err, afterFailure) || calls != 1 {
		t.Fatalf("committed delete did not reconcile on retry: err=%v calls=%d", err, calls)
	}
	if _, err := os.Lstat(filepath.Join(dest, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("retry did not clear the removed skill link: %v", err)
	}
	live.After = func() error { calls++; return nil }
	if err := live.RemoveSource(src.Slug); err != nil || calls != 2 {
		t.Fatalf("pending delete did not finish: err=%v calls=%d", err, calls)
	}
	if err := live.RemoveSource(src.Slug); err == nil || calls != 2 {
		t.Fatalf("completed delete retained retry authority: err=%v calls=%d", err, calls)
	}
}

func newTestLive(t *testing.T) (*Live, string) {
	t.Helper()
	root := t.TempDir()
	m, err := Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "runtime")
	return &Live{Map: m, Dests: []string{dest}}, dest
}

func assertLiveLink(t *testing.T, dest, name, want string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(dest, name))
	if err != nil || got != want {
		t.Fatalf("link %s = %q, err = %v; want %q", name, got, err, want)
	}
}
