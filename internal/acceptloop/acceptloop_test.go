package acceptloop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// scripted returns its errors from Accept in turn, then accepts from the
// listener it wraps.
type scripted struct {
	net.Listener
	errs []error
}

func (l *scripted) Accept() (net.Conn, error) {
	if len(l.errs) > 0 {
		err := l.errs[0]
		l.errs = l.errs[1:]
		return nil, err
	}
	return l.Listener.Accept()
}

func acceptError(errno syscall.Errno) error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", errno)}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// Temporary failures are waited out: the loop goes on to hand out the
// connection that arrives after them, having paused 5ms, 10ms and 20ms.
func TestRunAcceptsAgainAfterTemporaryFailures(t *testing.T) {
	raw := listen(t)
	l := &scripted{Listener: raw, errs: []error{acceptError(syscall.EMFILE), acceptError(syscall.ENFILE), fmt.Errorf("wrapped: %w", syscall.ECONNABORTED)}}
	handled := make(chan net.Conn, 1)
	ran := make(chan error, 1)
	start := time.Now()
	go func() {
		ran <- Run(t.Context(), l, "test", func(conn net.Conn) { handled <- conn })
	}()
	dialed, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	select {
	case conn := <-handled:
		conn.Close()
	case err := <-ran:
		t.Fatalf("Run returned %v on a temporary failure", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no connection handed out after the temporary failures")
	}
	if paused := time.Since(start); paused < 35*time.Millisecond {
		t.Fatalf("accepted after %v; three failures pause at least 35ms", paused)
	}
	raw.Close()
	if err := <-ran; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Run returned %v once its listener closed, want net.ErrClosed", err)
	}
}

// A failure accepting again would not cure ends the loop with that error.
func TestRunReturnsAPermanentFailure(t *testing.T) {
	broken := acceptError(syscall.EINVAL)
	l := &scripted{Listener: listen(t), errs: []error{broken}}
	if err := Run(t.Context(), l, "test", func(net.Conn) { t.Error("handed out a connection") }); err != broken {
		t.Fatalf("Run returned %v, want %v", err, broken)
	}
}

// failing fails every Accept with EMFILE and reports each call on calls.
type failing struct {
	net.Listener
	calls chan struct{}
}

func (l *failing) Accept() (net.Conn, error) {
	l.calls <- struct{}{}
	return nil, acceptError(syscall.EMFILE)
}

// ctx ending cuts a pause short, so a stop does not wait out up to a
// second of backoff before the loop returns. The ninth failure in a row
// pauses a full second; ctx ends well inside it, and the loop returns
// without accepting again, which it would only do once the pause ran out.
func TestRunReturnsWhenCtxEndsDuringAPause(t *testing.T) {
	l := &failing{Listener: listen(t), calls: make(chan struct{}, 16)}
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan error, 1)
	go func() { ran <- Run(ctx, l, "test", func(net.Conn) {}) }()
	for range 9 {
		select {
		case <-l.calls:
		case err := <-ran:
			t.Fatalf("Run returned %v on a temporary failure", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the loop stopped accepting before its ninth failure")
		}
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-ran:
		if !errors.Is(err, syscall.EMFILE) {
			t.Fatalf("Run returned %v, want the failure it paused on", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after ctx ended")
	}
	if accepts := 9 + len(l.calls); accepts != 9 {
		t.Fatalf("the loop accepted %d times, again after ctx ended during its pause", accepts)
	}
}

// A run of failures is logged once, not once per failure.
func TestRunWarnsOnceForARunOfFailures(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	raw := listen(t)
	l := &scripted{Listener: raw, errs: []error{acceptError(syscall.EMFILE), acceptError(syscall.EMFILE), acceptError(syscall.EMFILE)}}
	handled := make(chan struct{})
	ran := make(chan error, 1)
	go func() {
		ran <- Run(t.Context(), l, "test listener", func(conn net.Conn) { conn.Close(); close(handled) })
	}()
	dialed, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dialed.Close()
	select {
	case <-handled:
	case err := <-ran:
		t.Fatalf("Run returned %v on a temporary failure", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no connection handed out after the temporary failures")
	}
	raw.Close()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return once its listener closed")
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("logged %d warnings for three failures, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "test listener: accept:") {
		t.Fatalf("the warning does not name the listener:\n%s", logs.String())
	}
}
