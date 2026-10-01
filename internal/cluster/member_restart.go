package cluster

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// MemberRestarts is the active coordinator's port to the members that own
// machine links. It carries public operation identity, never SSH credentials.
type MemberRestarts interface {
	Find(context.Context, string, string) (RestartTarget, error)
	Start(context.Context, attempt.ForceRestart) error
	Status(context.Context, attempt.ForceRestart) (sshconnect.MemberRestart, error)
}

type RestartTarget struct{ ClusterID, Holder string }
type MemberRestartError struct{ Reason string }

func (e MemberRestartError) Error() string { return e.Reason }

type memberRestarts struct {
	peer   *Peer
	active Activation
}

func (p *Peer) MemberRestarts(active Activation) MemberRestarts { return memberRestarts{p, active} }
func (m memberRestarts) Find(context.Context, string, string) (RestartTarget, error) {
	return RestartTarget{}, errors.New("member restart is unavailable")
}
func (m memberRestarts) Start(context.Context, attempt.ForceRestart) error {
	return errors.New("member restart is unavailable")
}
func (m memberRestarts) Status(context.Context, attempt.ForceRestart) (sshconnect.MemberRestart, error) {
	return sshconnect.MemberRestart{}, errors.New("member restart is unavailable")
}
