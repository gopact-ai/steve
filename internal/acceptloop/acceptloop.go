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
// up to 1s, and starts over once a connection is accepted. The failure
// is logged as a warning naming what, at most once a minute. ctx ending
// during a pause returns the failure paused on.
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

// warnings logs the temporary failures of one accept loop.
type warnings struct {
	what   string
	warned time.Time // when the last warning was logged
}

// failed logs err, which the loop waits out for pause, unless a warning
// was logged less than warnEvery before now.
func (w *warnings) failed(now time.Time, err error, pause time.Duration) {
	if w.warned.IsZero() || now.Sub(w.warned) >= warnEvery {
		w.warned = now
		slog.Warn(fmt.Sprintf("%s: accept: %v; accepting again in %v", w.what, err, pause))
	}
}

// accepted notes a connection accepted at now; it logs nothing.
func (w *warnings) accepted(now time.Time) {}

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
