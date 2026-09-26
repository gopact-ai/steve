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
	// warnEvery bounds how often a run of failures is logged; while they
	// last, one comes at most every maxPause.
	warnEvery = time.Minute
)

// Run accepts from l and hands each connection to handle, which should
// return promptly. It returns the first Accept error that accepting again
// would not cure, including the one a closed listener reports; the caller
// tells a stop it asked for from a failure.
//
// A temporary failure — the process or system out of descriptors or
// buffers, or a connection aborted before it was accepted — pauses and
// accepts again. The pause starts at 5ms, doubles up to 1s, and starts
// over once a connection is accepted. The failure is logged as a warning
// naming what, at most once a minute. ctx ending during a pause returns
// the failure paused on.
func Run(ctx context.Context, l net.Listener, what string, handle func(net.Conn)) error {
	var pause time.Duration
	var warned time.Time
	for {
		conn, err := l.Accept()
		if err == nil {
			pause = 0
			handle(conn)
			continue
		}
		if !temporary(err) || ctx.Err() != nil {
			return err
		}
		pause = min(max(2*pause, firstPause), maxPause)
		if now := time.Now(); warned.IsZero() || now.Sub(warned) >= warnEvery {
			warned = now
			slog.Warn(fmt.Sprintf("%s: accept: %v; accepting again in %v", what, err, pause))
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

// temporary reports whether accepting again may succeed after err. These
// are the errors accept(2) returns for resources that free up, and for a
// connection that went away before it was accepted.
func temporary(err error) bool {
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
