// Package peerstop terminates only the peer holding one identified installation's
// lock. The caller cannot supply a PID or a process-group number.
package peerstop

import (
	"context"
	"errors"
)

var (
	ErrUnsupported = errors.New("stable peer process handles are unsupported")
	ErrUnproven    = errors.New("original peer installation or process cannot be proved")
	ErrRunning     = errors.New("original peer is still running")
)

func Stop(ctx context.Context, sidecar, cluster, node string) (bool, error) {
	return stopInstallation(ctx, sidecar, cluster, node)
}
