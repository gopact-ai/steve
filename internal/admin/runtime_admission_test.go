package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestLazyHubPendingBatchCannotStartANewHarness(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, change := range []string{"command", "symlink", "hardlink", "removed tool"} {
		t.Run(change, func(t *testing.T) {
			a := hubNodeSettingsFixture(t)
			dir := t.TempDir()
			slow, started, release := filepath.Join(dir, "slow-tool"), filepath.Join(dir, "started"), filepath.Join(dir, "release")
			candidate, counter, tail := filepath.Join(dir, "candidate"), filepath.Join(dir, "counter"), filepath.Join(dir, "ordinary-tail")
			if err := os.WriteFile(slow, []byte(fmt.Sprintf("#!/bin/sh\necho start > '%s'\nwhile [ ! -e '%s' ]; do sleep 0.01; done\necho 'host 1.2.3'\n", started, release)), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
			if err := os.WriteFile(candidate, []byte("#!/bin/sh\necho start >> '"+counter+"'\necho 'candidate 1.2.3'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tail, []byte("#!/bin/sh\necho 'tail 1.2.3'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			registered := candidate
			if change == "symlink" || change == "hardlink" {
				registered = filepath.Join(dir, "agent-alias")
				var err error
				if change == "symlink" {
					err = os.Symlink(candidate, registered)
				} else {
					err = os.Link(candidate, registered)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			a.cfg().Gateway.Tools = []string{slow, candidate, tail}
			a.Observation = NewLocalObservation(node.NewLaunchProbe())
			ctx, cancel := context.WithCancel(t.Context())
			stopped := make(chan struct{})
			go func() { defer close(stopped); a.Observation.RunLaunchProbe(ctx, a.ConfigStore) }()
			t.Cleanup(func() { cancel(); <-stopped })
			wait := func(ready func() bool) {
				t.Helper()
				until := time.Now().Add(4 * time.Second)
				for time.Now().Before(until) {
					if ready() {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
				t.Fatal("host tool pass did not reach barrier")
			}
			wait(func() bool { _, err := os.Stat(started); return err == nil })
			set, err := a.NodeSettings(t.Context(), a.NodeName)
			if err != nil {
				t.Fatal(err)
			}
			if change == "removed tool" {
				set.Tools = []string{slow, tail}
			} else {
				set.Harnesses["custom"] = nodewire.HarnessSetting{Command: registered, Args: []string{"acp", ""}, Env: []string{"MODE=ordinary"}}
			}
			saved := make(chan error, 1)
			go func() { _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); saved <- err }()
			select {
			case err := <-saved:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("config writer was held across host-tool Wait")
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			wait(func() bool { _, ok := a.Observation.Launch.Lookup(tail); return ok })
			if raw, err := os.ReadFile(counter); err == nil && strings.Count(string(raw), "\n") > 0 {
				t.Fatalf("published v2 still launched queued candidate: %q", raw)
			} else if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if _, cached := a.Observation.Launch.Lookup(candidate); cached {
				t.Fatal("denied candidate acquired launch evidence")
			}
		})
	}
}

func TestLazyHubAdmissionStartAndPublicationAreAtomic(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "old-tool")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Harnesses: map[string]config.Harness{}}
	cfg.Gateway.Tools = []string{program}
	store := NewConfigStore(cfg)
	cmd := exec.Command(program)
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	reached, release, admitted := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		ok, err := store.admitLaunch(0, func() error { close(reached); <-release; return cmd.Start() })
		if !ok && err == nil {
			err = fmt.Errorf("v1 host tool denied")
		}
		admitted <- err
	}()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	<-reached
	if store.mu.TryLock() {
		store.mu.Unlock()
		t.Fatal("permission check and Start left a publication window")
	}
	saved := make(chan error, 1)
	go func() {
		saved <- store.Update(func(c *config.Config) error { c.Harnesses["custom"] = config.Harness{Command: program}; return nil }, func(*config.Config) error { return nil })
	}()
	select {
	case err := <-saved:
		t.Fatalf("v2 published before admitted Start: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("publication still blocked after Start returned")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestLazyHubPublicationEpochSkipsStaleButNotFailedSaves(t *testing.T) {
	cfg := &config.Config{Harnesses: map[string]config.Harness{}}
	store := NewConfigStore(cfg)
	snapshot, initial := store.launchSnapshot()
	if snapshot == nil {
		t.Fatal("missing initial snapshot")
	}
	called := 0
	start := func() error { called++; return nil }
	fail := errors.New("fixture save refused")
	err := store.Update(func(c *config.Config) error { c.Gateway.Tools = []string{"ordinary"}; return nil }, func(*config.Config) error { return fail })
	if !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if ok, err := store.admitLaunch(initial, start); !ok || err != nil {
		t.Fatal("failed save invalidated live admission")
	}
	err = store.Update(func(c *config.Config) error { c.Harnesses["custom"] = config.Harness{Command: "candidate"}; return nil }, func(*config.Config) error { return &config.CommittedError{Err: fail} })
	if !config.Committed(err) {
		t.Fatalf("committed outcome = %v", err)
	}
	if ok, _ := store.admitLaunch(initial, start); ok {
		t.Fatal("old epoch admitted after committed publication")
	}
	current, next := store.launchSnapshot()
	if next == initial || current.Harnesses["custom"].Command != "candidate" {
		t.Fatal("published state/epoch mismatch")
	}
	if _, changed := snapshot.Harnesses["custom"]; changed {
		t.Fatal("planning snapshot was mutated in place")
	}
	if ok, err := store.admitLaunch(next, start); !ok || err != nil {
		t.Fatal("current epoch refused")
	}
	if called != 2 {
		t.Fatalf("Start invoked %d times, want only failed-save live + current", called)
	}
}
