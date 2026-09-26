// Package acceptloop runs a listener's accept loop through the failures a
// busy process recovers from, the way net/http's Server does.
package acceptloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"syscall"
	"time"
)

const (
	firstPause = 5 * time.Millisecond
	maxPause   = time.Second
	// warnEvery bounds how often a failure is logged.
	warnEvery = time.Minute
)

// Run accepts from l and hands each connection to handle, which should
// return promptly. It returns the first Accept error that accepting again
// would not cure, including the one a closed listener reports; the caller
// tells a stop it asked for from a failure.
//
// A temporary failure — the process or system out of descriptors or
// buffers, or a connection that failed or went away before it was
// accepted — pauses and accepts again. The pause starts at 5ms, doubles
// up to 1s, and starts over once a connection is accepted. ctx ending
// during a pause returns the failure paused on.
//
// Failures are logged as warnings naming what, at most one a minute.
// Those that come sooner are counted, and the count goes out with the
// next failure or accept once the minute is up. A run of failures that
// was warned about ends with a line saying what accepts again.
func Run(ctx context.Context, l net.Listener, what string, handle func(net.Conn)) error {
	var pause time.Duration
	w := warnings{what: what}
	for {
		conn, err := l.Accept()
		if err == nil {
			pause = 0
			w.accepted(time.Now())
			handle(conn)
			continue
		}
		if !temporary(err) || ctx.Err() != nil {
			return err
		}
		pause = min(max(2*pause, firstPause), maxPause)
		w.failed(time.Now(), err, pause)
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

// warnings logs the temporary failures of one accept loop: at most one
// warning every warnEvery, however failures and accepts alternate, and
// one line ending each run of failures that had a warning. Failures not
// logged are counted, and a later line carries the count, so a run is
// never left out because it came soon after another.
type warnings struct {
	what     string
	warned   time.Time // when the last warning was logged
	unlogged int       // failures since the last line, none of them in it
	last     error     // the latest of those failures
	run      int       // failures since the last accept
	told     bool      // whether the run has had a warning
}

// failed logs err, which the loop waits out for pause, if the last
// warning was warnEvery or more before now, and otherwise counts it.
func (w *warnings) failed(now time.Time, err error, pause time.Duration) {
	w.run++
	w.unlogged++
	w.last = err
	if !w.due(now) {
		return
	}
	msg := fmt.Sprintf("%s: accept: %v; accepting again in %v", w.what, err, pause)
	if earlier := w.unlogged - 1; earlier > 0 {
		msg += fmt.Sprintf(" (%s before it not logged)", failures(earlier))
	}
	w.warn(now, msg)
	w.told = true
}

// accepted ends a run of failures, logging its end if the run had a
// warning, and logs failures still uncounted once a warning is due.
func (w *warnings) accepted(now time.Time) {
	if w.told {
		slog.Info(fmt.Sprintf("%s: accepting again after %s", w.what, failures(w.run)))
		w.unlogged = 0
	}
	w.run, w.told = 0, false
	if w.unlogged > 0 && w.due(now) {
		w.warn(now, fmt.Sprintf("%s: accept: %v; %s not logged, accepting again", w.what, w.last, failures(w.unlogged)))
	}
}

func (w *warnings) due(now time.Time) bool {
	return w.warned.IsZero() || now.Sub(w.warned) >= warnEvery
}

func (w *warnings) warn(now time.Time, msg string) {
	slog.Warn(msg)
	w.warned, w.unlogged, w.last = now, 0, nil
}

func failures(n int) string {
	if n == 1 {
		return "1 failure"
	}
	return fmt.Sprintf("%d failures", n)
}

// temporaries are the errors accept(2) returns for resources that free
// up, and for a single connection that failed or went away before it was
// accepted: net/http waits out ECONNRESET and ECONNABORTED (Go's accept
// retries the latter itself on Unix) and ETIMEDOUT, and Linux passes on a
// pending connection's network error, the rest of the list and
// platformTemporaries, for the caller to retry. The listeners here are
// all stream sockets, so EOPNOTSUPP cannot mean the socket does not
// accept. On Windows these are Go's own values rather than what Winsock
// reports, so none matches and every failure ends the loop.
var temporaries = append([]syscall.Errno{
	syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM,
	syscall.ECONNABORTED, syscall.ECONNRESET, syscall.ETIMEDOUT,
	syscall.ENETDOWN, syscall.EPROTO, syscall.ENOPROTOOPT, syscall.EHOSTDOWN,
	syscall.EHOSTUNREACH, syscall.EOPNOTSUPP, syscall.ENETUNREACH,
}, platformTemporaries...)

// temporary reports whether accepting again may succeed after err.
func temporary(err error) bool {
	for _, errno := range temporaries {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
