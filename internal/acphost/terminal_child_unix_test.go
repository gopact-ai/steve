//go:build linux || darwin

package acphost

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
)

func TestTerminalInertChildHelperProcess(t *testing.T) {
	if os.Getenv("GO_TERMINAL_INERT_CHILD") != "1" {
		return
	}
	_, err := RunTerminalChild([]string{TerminalChildVerb})
	if err != nil {
		terminalChildError(err)
		os.Exit(2)
	}
	os.Exit(0)
}

func terminalChildTestOwner(t *testing.T) (*exec.Cmd, procgroup.Identity) {
	t.Helper()
	originalMark := procgroup.NewMark()
	owner := exec.Command("/bin/sleep", "30")
	owner.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	owner.Env = append(withoutEnv(os.Environ(), procgroup.MarkVariable), procgroup.MarkVariable+"="+originalMark)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := procgroup.Capture(owner.Process.Pid, originalMark)
	if err != nil {
		owner.Process.Kill()
		owner.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The original unreaped child pins its group ID until this cleanup.
		_ = procgroup.Kill(owner.Process.Pid)
		_ = owner.Wait()
	})
	return owner, id
}

func TestTerminalChildPayloadNeedsBothGates(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, original := terminalChildTestOwner(t)
	for _, stage := range []string{"preparing", "active", "execute"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			pinned, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.Close()
			control, gate, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			notice, writer, err := os.Pipe()
			if err != nil {
				control.Close()
				t.Fatal(err)
			}
			defer notice.Close()
			child := exec.Command(exe, "-test.run=^TestTerminalInertChildHelperProcess$")
			child.Env = append(withoutEnv(os.Environ(), procgroup.MarkVariable),
				procgroup.MarkVariable+"="+original.Mark, "GO_TERMINAL_INERT_CHILD=1")
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: original.Group}
			child.ExtraFiles = []*os.File{control, pinned, writer}
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			if err := child.Start(); err != nil {
				control.Close()
				writer.Close()
				t.Fatal(err)
			}
			control.Close()
			writer.Close()
			reaped := false
			t.Cleanup(func() {
				if !reaped {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			})
			mark := procgroup.NewMark()
			config := terminalChildConfig{
				Command: "/bin/sh", Args: []string{"-c", `printf 'payload' > marker.txt; printf 'terminal output'`},
				Env: []string{"PATH=/usr/bin:/bin"}, ParentGroup: original.Group,
				ParentMark: original.Mark, Mark: mark,
			}
			if err := writeTerminalChildGate(gate, config); err != nil {
				t.Fatal(err)
			}
			if err := notice.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(notice)
			if err := terminalNotice(reader, "preparing", child.Process.Pid); err != nil {
				t.Fatalf("%v; helper output: %s", err, output.String())
			}
			if _, err := os.Stat(filepath.Join(root, "marker.txt")); !os.IsNotExist(err) {
				t.Fatal("inert helper executed payload before a group gate")
			}
			preparation, err := procgroup.CapturePreparation(child.Process.Pid, original)
			if err != nil {
				t.Fatal(err)
			}
			if stage != "preparing" {
				if _, err := io.WriteString(gate, "SPLIT\n"); err != nil {
					t.Fatal(err)
				}
				if err := terminalNotice(reader, "active", child.Process.Pid); err != nil {
					t.Fatal(err)
				}
				id, err := preparation.CapturedGroup(mark)
				if err != nil || id.Group != child.Process.Pid || id.Start == 0 {
					t.Fatalf("split child lacks real leader identity: %+v %v", id, err)
				}
				if _, err := os.Stat(filepath.Join(root, "marker.txt")); !os.IsNotExist(err) {
					t.Fatal("group split executed payload before its active gate")
				}
			}
			if stage == "execute" {
				// Prove cwd uses the inherited directory FD, not a replaced
				// path. This is a helper contract test, not owner authorization.
				moved := root + "-moved"
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(moved)
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(gate, "EXEC\n"); err != nil {
					t.Fatal(err)
				}
				gate.Close()
				done := make(chan error, 1)
				go func() { done <- child.Wait() }()
				select {
				case err := <-done:
					reaped = true
					if err != nil {
						t.Fatalf("admitted helper payload failed: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("helper payload did not exit")
				}
				raw, err := os.ReadFile(filepath.Join(moved, "marker.txt"))
				if err != nil || string(raw) != "payload" || output.String() != "terminal output" {
					t.Fatalf("payload output or pinned cwd differs: %q %v output=%q", raw, err, output.String())
				}
				if _, err := os.Stat(filepath.Join(root, "marker.txt")); !os.IsNotExist(err) {
					t.Fatal("payload entered a replacement cwd path")
				}
				return
			}
			gate.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- child.Wait() }()
			select {
			case err := <-done:
				reaped = true
				if err == nil {
					t.Fatal("missing payload gate was reported successful")
				}
			case <-ctx.Done():
				t.Fatal("gate EOF did not end inert helper")
			}
			if _, err := os.Stat(filepath.Join(root, "marker.txt")); !os.IsNotExist(err) {
				t.Fatal("gate EOF executed payload")
			}
		})
	}
}
