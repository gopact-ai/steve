package cluster

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

var errNodeRestartPending = errors.New("node restart is not available yet")

func (b peerSSHBackend) RestartTarget(context.Context, string) (string, error) {
	return "", errNodeRestartPending
}

func (b peerSSHBackend) Restarted(context.Context, string) error { return errNodeRestartPending }

func (b peerSSHBackend) RecordRestart(context.Context, sshconnect.RestartRecord) error {
	return errNodeRestartPending
}

func (b peerSSHBackend) Watched(context.Context) []string { return nil }

func (b peerSSHBackend) Answers(context.Context, string) bool { return false }

func (b peerSSHBackend) Reachable(context.Context, string) bool { return false }

func awaitAnswerWithin(context.Context, func(context.Context) (string, error), time.Duration, time.Duration) error {
	return errNodeRestartPending
}

func (p *Peer) serveNodeRestart(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
