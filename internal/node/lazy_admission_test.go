package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestLazyNodePendingBatchCannotStartANewHarness(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, change := range []string{"command", "symlink", "hardlink", "removed tool"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
			slow := filepath.Join(dir, "slow-tool")
			body := fmt.Sprintf("#!/bin/sh\necho started > '%s'\nwhile [ ! -e '%s' ]; do sleep 0.01; done\necho 'host tool 1.2.3'\n", started, release)
			if err := os.WriteFile(slow, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
			candidate, counter := lazyCounterProgram(t, "candidate")
			tail, tailCounter := lazyCounterProgram(t, "ordinary-tail")
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
			cfg := ServerConfig{Name: "fixture-node", Token: "fixture-only", Source: filepath.Join(t.TempDir(), "node.json"), StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Tools: []string{slow, candidate, tail}, Harnesses: map[string]HarnessSpec{}}
			if err := writeConfig(cfg); err != nil {
				t.Fatal(err)
			}
			server := startNode(t, cfg)
			lazyWait(t, func() bool { _, err := os.Stat(started); return err == nil })
			set := server.settings()
			if change == "removed tool" {
				set.Tools = []string{slow, tail}
			} else {
				set.Harnesses["custom"] = nodewire.HarnessSetting{Command: registered, Args: []string{"acp", ""}, Env: []string{"MODE=ordinary"}}
			}
			done := make(chan error, 1)
			go func() { done <- server.applySettings(set) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("settings save was held across slow host-tool Wait")
			}
			if change != "removed tool" && server.conf().Harnesses["custom"].Command != registered {
				t.Fatal("v2 was not published")
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			lazyWait(t, func() bool { _, ok := server.launch.Lookup(tail); return ok })
			if count := lazyStarts(t, counter); count != 0 {
				t.Fatalf("published v2 still started old candidate %d times", count)
			}
			if _, cached := server.launch.Lookup(candidate); cached {
				t.Fatal("denied queued candidate acquired launch evidence")
			}
			if count := lazyStarts(t, tailCounter); count == 0 {
				t.Fatal("same-version ordinary tail lost its check")
			}
		})
	}
}

func TestLazyNodePendingBatchUsesCurrentToolSelection(t *testing.T) {
	// The first test's "removed tool" case exercises this through normal save.
	// Keep an ordinary fixed-config pass / dedupe baseline beside admission.
	tool, counter := lazyCounterProgram(t, "ordinary")
	server := NewServer(ServerConfig{Tools: []string{tool, tool}, Harnesses: map[string]HarnessSpec{}})
	server.launch.pass(t.Context(), server.commands())
	raw, err := os.ReadFile(counter)
	if err != nil || strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("ordinary tool/dedupe changed: %q %v", raw, err)
	}
}

func TestLazyNodeAdmissionStartAndPublicationAreAtomic(t *testing.T) {
	program, counter := lazyCounterProgram(t, "old-tool")
	cfg := ServerConfig{Source: filepath.Join(t.TempDir(), "node.json"), Name: "fixture-node", Tools: []string{program}, Harnesses: map[string]HarnessSpec{}}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(cfg)
	cmd := exec.Command(program)
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	reached, release, admitted := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		ok, err := server.admitToolLaunch(server.cfg.Load(), func() error { close(reached); <-release; return cmd.Start() })
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
	if server.settingsMu.TryLock() {
		server.settingsMu.Unlock()
		t.Fatal("classification and cmd.Start are separated by an unlocked publication window")
	}
	set := server.settings()
	set.Harnesses["custom"] = nodewire.HarnessSetting{Command: program, Args: []string{"acp"}}
	saved := make(chan error, 1)
	go func() { saved <- server.applySettings(set) }()
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
	if lazyStarts(t, counter) != 1 {
		t.Fatal("the already-admitted v1 host tool did not start")
	}
}

func TestLazyAdmissionExpiredBeforeStartDoesNotInventLaunchEvidence(t *testing.T) {
	program, counter := lazyCounterProgram(t, "not-started")
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	result, dispatched := launch(ctx, program, func(_ string, start func() error) (bool, error) { return true, start() })
	if dispatched || result.OK || lazyStarts(t, counter) != 0 {
		t.Fatalf("expired admission claimed a start: %+v dispatched=%v", result, dispatched)
	}
}

func TestLazyBackgroundWorkerRequiresAdmissionButExplicitLaunchStillWorks(t *testing.T) {
	program, counter := lazyCounterProgram(t, "explicit-host-tool")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	probe := NewLaunchProbe()
	probe.Run(ctx, func() []string { return []string{program} }, nil)
	if lazyStarts(t, counter) != 0 {
		t.Fatal("background worker without a configuration owner started a process")
	}
	if result := Launch(t.Context(), program); !result.OK || lazyStarts(t, counter) != 1 {
		t.Fatalf("explicit tool launch was disabled: %+v", result)
	}
}
