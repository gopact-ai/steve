//go:build linux || darwin

package acphost

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/procgroup"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func TestReapedGroupKillRequiresItsRecordedIdentity(t *testing.T) {
	for _, cause := range []error{nil, procgroup.ErrUnproven, procgroup.ErrRunning} {
		t.Run(func() string {
			if cause == nil {
				return "confirmed"
			}
			return cause.Error()
		}(), func(t *testing.T) {
			here, err := procgroup.Here()
			if err != nil {
				t.Skip(err)
			}
			id := procgroup.Identity{Group: 123, Leader: 123, Start: 42, Mark: "original"}
			calls := 0
			p := &localProcess{reaped: true, identity: id, place: here, group: groupCalls{kill: func(int) error { t.Fatal("raw group signal after leader was reaped"); return nil }, settle: func(got procgroup.Identity, ran, now procgroup.Place, within time.Duration) error {
				calls++
				if got != id || ran != here || now != here || within > time.Second {
					t.Fatal("recorded identity changed")
				}
				return cause
			}}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err = p.KillNow(ctx)
			if !errors.Is(err, cause) || calls != 1 {
				t.Fatalf("kill=%v calls=%d", err, calls)
			}
			if p.Stopped() != (cause == nil) {
				t.Fatal("force kill lost the original identity's settlement evidence")
			}
		})
	}
}
func TestHostKillSkipsNativeInitializationGrace(t *testing.T) {
	started := make(chan struct{})
	var once atomic.Bool
	h := New(Config{Transport: LocalTransport{Command: "/bin/sh", Args: []string{"-c", "exec sleep 30"}, ProcessDir: t.TempDir(), Started: func(procgroup.Identity) {
		if once.CompareAndSwap(false, true) {
			close(started)
		}
	}}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(h.Close)
	opened := make(chan error, 1)
	go func() { _, _, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()}); opened <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture did not start")
	}
	// Cancelling initialization frees the host lock before its graceful shutdown
	// wait; force stop must use that opportunity, not wait out the grace.
	began := time.Now()
	cancel()
	if err := h.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if time.Since(began) > 3*time.Second || !h.AllProcessesStopped() {
		t.Fatal("force stop waited for initialization grace")
	}
	<-opened
}
func TestReapedGroupWithReusedLeaderIsNeverSignalled(t *testing.T) {
	// Use only this test's child. The stale start time makes the identity
	// deliberately different; a successful proof means that old group is gone.
	cmd := exec.Command("/bin/sh", "-c", "sleep 10")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { killProcessGroup(cmd); _ = cmd.Wait() }()
	here, err := procgroup.Here()
	if err != nil {
		t.Skip(err)
	}
	id, err := procgroup.Capture(cmd.Process.Pid, "old-mark")
	if err != nil {
		t.Fatal(err)
	}
	id.Start++
	p := &localProcess{reaped: true, identity: id, place: here}
	if err := p.KillNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	remains, err := procgroup.Inspect(cmd.Process.Pid)
	if err != nil || remains.Running == 0 {
		t.Fatalf("reused identity killed unrelated live group: %+v %v", remains, err)
	}
}
