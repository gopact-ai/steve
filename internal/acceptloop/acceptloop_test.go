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

// Accept failures that accepting again may cure are temporary: the
// process or system out of descriptors or buffers, and a connection that
// failed or went away before it was accepted, which net/http also waits
// out and Linux's accept(2) says to retry. Anything else, a closed
// listener included, is not.
func TestTemporaryAcceptFailures(t *testing.T) {
	temporaries := append([]syscall.Errno{
		syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM,
		syscall.ECONNABORTED, syscall.ECONNRESET, syscall.ETIMEDOUT,
		syscall.ENETDOWN, syscall.EPROTO, syscall.ENOPROTOOPT, syscall.EHOSTDOWN,
		syscall.EHOSTUNREACH, syscall.EOPNOTSUPP, syscall.ENETUNREACH,
	}, linuxOnlyTemporaries...)
	for _, errno := range temporaries {
		if !temporary(acceptError(errno)) {
			t.Errorf("%v (%d) ends the loop; accepting again may cure it", errno, uintptr(errno))
		}
	}
	for _, err := range []error{acceptError(syscall.EINVAL), acceptError(syscall.EBADF), acceptError(syscall.ENOTSOCK), net.ErrClosed, &net.OpError{Op: "accept", Net: "tcp", Err: net.ErrClosed}} {
		if temporary(err) {
			t.Errorf("%v is waited out; accepting again would not cure it", err)
		}
	}
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

// A run of failures is logged once, not once per failure, and the
// accept that ends it logs that the listener accepts again.
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
	if n := strings.Count(logs.String(), "level=INFO msg=\"test listener: accepting again after 3 failures\""); n != 1 {
		t.Fatalf("the accept that ends the run is not logged once:\n%s", logs.String())
	}
}

// logged captures what the default logger writes for the rest of the
// test and returns a function that yields the lines written so far.
func logged(t *testing.T) func() []string {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []string {
		return strings.Split(strings.TrimSpace(logs.String()), "\n")
	}
}

// lines counts the lines at level that contain every one of parts.
func lines(logs []string, level string, parts ...string) int {
	n := 0
	for _, line := range logs {
		matches := strings.Contains(line, "level="+level)
		for _, part := range parts {
			matches = matches && strings.Contains(line, part)
		}
		if matches {
			n++
		}
	}
	return n
}

// A run of failures that starts and ends within a minute of the last
// warning is still logged: its failures are counted, and the first
// accept once the minute is up logs a warning with that count.
func TestALaterRunOfFailuresIsLogged(t *testing.T) {
	logs := logged(t)
	start := time.Now()
	w := warnings{what: "test listener"}
	w.failed(start, acceptError(syscall.EMFILE), firstPause)
	w.accepted(start.Add(10 * time.Millisecond))
	w.failed(start.Add(20*time.Second), acceptError(syscall.ENFILE), firstPause)
	w.failed(start.Add(20*time.Second+firstPause), acceptError(syscall.ENFILE), 2*firstPause)
	w.accepted(start.Add(20*time.Second + 3*firstPause))
	w.accepted(start.Add(30 * time.Second))
	if n := lines(logs(), "WARN"); n != 1 {
		t.Fatalf("logged %d warnings within a minute, want 1:\n%s", n, strings.Join(logs(), "\n"))
	}
	w.accepted(start.Add(warnEvery))
	if n := lines(logs(), "WARN", "test listener: accept:", syscall.ENFILE.Error(), "2 failures not logged"); n != 1 {
		t.Fatalf("the second run of failures is not logged once the minute is up:\n%s", strings.Join(logs(), "\n"))
	}
	w.accepted(start.Add(3 * warnEvery))
	if n := lines(logs(), "WARN"); n != 2 {
		t.Fatalf("logged %d warnings, want one for each run of failures:\n%s", n, strings.Join(logs(), "\n"))
	}
}

// A warned run of failures ends with a line saying the listener accepts
// again and how many failures the run had, so a reader knows the
// incident is over.
func TestAWarnedRunEndsWithALineSayingSo(t *testing.T) {
	logs := logged(t)
	start := time.Now()
	w := warnings{what: "test listener"}
	for i := range 3 {
		w.failed(start.Add(time.Duration(i)*time.Second), acceptError(syscall.EMFILE), time.Second)
	}
	w.accepted(start.Add(3 * time.Second))
	w.accepted(start.Add(4 * time.Second))
	if n := lines(logs(), "INFO", "test listener: accepting again after 3 failures"); n != 1 {
		t.Fatalf("the end of the run is not logged once:\n%s", strings.Join(logs(), "\n"))
	}
	w.accepted(start.Add(2 * warnEvery))
	if n := lines(logs(), "WARN"); n != 1 {
		t.Fatalf("logged %d warnings for one run the end of which was logged, want 1:\n%s", n, strings.Join(logs(), "\n"))
	}
}

// While failures last, a warning comes once a minute and counts the
// failures since the last line.
func TestALongRunOfFailuresIsCountedEveryMinute(t *testing.T) {
	logs := logged(t)
	start := time.Now()
	w := warnings{what: "test listener"}
	for i := range 181 {
		w.failed(start.Add(time.Duration(i)*time.Second), acceptError(syscall.EMFILE), time.Second)
	}
	if n := lines(logs(), "WARN"); n != 4 {
		t.Fatalf("logged %d warnings over three minutes of failures, want 4:\n%s", n, strings.Join(logs(), "\n"))
	}
	if n := lines(logs(), "WARN", "(59 failures before it not logged)"); n != 3 {
		t.Fatalf("the warnings after the first do not count the failures in between:\n%s", strings.Join(logs(), "\n"))
	}
}

// Failures and accepts that alternate log at most a warning and the line
// ending its run each minute, however often they alternate.
func TestAlternatingFailuresAndAcceptsLogBoundedLines(t *testing.T) {
	logs := logged(t)
	start := time.Now()
	w := warnings{what: "test listener"}
	const minutes = 10
	for at := time.Duration(0); at < minutes*warnEvery; at += 20 * time.Millisecond {
		w.failed(start.Add(at), acceptError(syscall.EMFILE), firstPause)
		w.accepted(start.Add(at + 10*time.Millisecond))
	}
	all := logs()
	if n := lines(all, "WARN"); n > minutes {
		t.Fatalf("logged %d warnings over %d minutes, want at most one a minute", n, minutes)
	}
	if n := lines(all, "INFO"); n > minutes {
		t.Fatalf("logged %d lines ending a run over %d minutes, want at most one a minute", n, minutes)
	}
	if n := lines(all, "WARN", "failures before it not logged"); n != minutes-1 {
		t.Fatalf("%d warnings count the failures since the line before, want %d:\n%s", n, minutes-1, strings.Join(all, "\n"))
	}
}
