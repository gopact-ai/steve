//go:build linux || darwin

package acphost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
)

func nativeTerminalParent(t *testing.T) *localProcess {
	t.Helper()
	var id procgroup.Identity
	proc, err := (LocalTransport{
		Command: "/bin/sleep", Args: []string{"30"},
		Env: []string{"GO_TERMINAL_INERT_CHILD=1"}, Started: func(identity procgroup.Identity) { id = identity },
	}).Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parent := proc.(*localProcess)
	if id.Group == 0 || parent.identity != id {
		parent.Kill()
		parent.Wait()
		t.Fatal("real parent process lacks its original native group identity")
	}
	t.Cleanup(func() { parent.Kill(); parent.Wait() })
	return parent
}

func TestTerminalPreparationUsesOriginalProcessAndPinnedRoot(t *testing.T) {
	parent := nativeTerminalParent(t)
	root := t.TempDir()
	cwd, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cwd.Close()
	output := newTerminalOutput(64)
	helper, err := parent.prepareTerminal(t.Context(), cwd, terminalChildConfig{
		Command: "/bin/sh", Args: []string{"-c", `printf payload > marker; printf terminal-output`},
		Env: []string{"PATH=/usr/bin:/bin"}, Mark: procgroup.NewMark(),
	}, output, []string{"-test.run=^TestTerminalInertChildHelperProcess$"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		helper.mu.Lock()
		executed := helper.ended
		helper.mu.Unlock()
		if !executed {
			_ = helper.endInert()
		}
	})
	if helper.preparation.ParentGroup != parent.identity.Group || helper.preparation.PID == parent.identity.Group {
		t.Fatal("inert helper did not belong to the exact original Agent group")
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		t.Fatal("helper preparation executed payload")
	}
	identity, err := helper.split(t.Context())
	if err != nil || identity.Group == parent.identity.Group || identity.Start != helper.preparation.Start {
		t.Fatalf("real captured split identity differs: %+v %v", identity, err)
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		t.Fatal("helper split executed payload before the payload gate")
	}
	moved := root + "-original"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := helper.exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- helper.cmd.Wait() }()
	select {
	case err := <-done:
		helper.mu.Lock()
		helper.waited = true
		helper.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		_ = helper.cmd.Process.Kill()
		<-done
		t.Fatal("terminal test payload did not finish")
	}
	helper.status.Close()
	raw, err := os.ReadFile(filepath.Join(moved, "marker"))
	text, truncated := output.Snapshot()
	if err != nil || string(raw) != "payload" || text != "terminal-output" || truncated {
		t.Fatalf("payload or captured output differs: %q %v %q %v", raw, err, text, truncated)
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		t.Fatal("payload was redirected to a replaced cwd pathname")
	}
	if err := helper.exec(t.Context()); err == nil {
		t.Fatal("the same helper payload gate was consumed twice")
	}
	select {
	case <-parent.Exited():
		t.Fatal("independent terminal execution killed its Agent")
	default:
	}
}

func TestTerminalPreparationClosesAdmissionBeforeAgentStop(t *testing.T) {
	parent := nativeTerminalParent(t)
	cwd, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cwd.Close()
	helper, err := parent.prepareTerminal(t.Context(), cwd, terminalChildConfig{
		Command: "/bin/true", Env: []string{"PATH=/usr/bin:/bin"}, Mark: procgroup.NewMark(),
	}, newTerminalOutput(32), []string{"-test.run=^TestTerminalInertChildHelperProcess$"})
	if err != nil {
		t.Fatal(err)
	}
	defer helper.endInert()
	parent.closeTerminalAdmission()
	if _, err := helper.split(t.Context()); err == nil {
		t.Fatal("closing Agent admitted a new independent group")
	}
	if _, err := parent.prepareTerminal(t.Context(), cwd, terminalChildConfig{
		Command: "/bin/true", Env: []string{"PATH=/usr/bin:/bin"}, Mark: procgroup.NewMark(),
	}, newTerminalOutput(32), []string{"-test.run=^TestTerminalInertChildHelperProcess$"}); err == nil {
		t.Fatal("closed original-generation admission started another helper")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := parent.KillNow(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parent.Exited():
	case <-ctx.Done():
		t.Fatal("original parent did not exit after its own stop")
	}
}
