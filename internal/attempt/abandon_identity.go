package attempt

import (
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
)

// CheckAbandonProjectionTx fences retirement of the recorded native context
// against a later binding, in the transaction that writes the session document.
func (s *Service) CheckAbandonProjectionTx(tx *ledger.Tx, expected Record) error {
	current, err := GetTx(tx, expected.ID)
	if err != nil {
		return err
	}
	if current.Abandoned == nil || expected.Abandoned == nil || current.Abandoned.ForceStopRevision != expected.Abandoned.ForceStopRevision || !current.Abandoned.At.Equal(expected.Abandoned.At) {
		return ErrForceStopChanged
	}
	if current.Abandoned.Session == "" {
		return nil
	}
	latest, err := latestSessionIdentity(tx, sessionIdentityKey(current.Node, current.Harness, current.Abandoned.Session))
	if err != nil {
		return err
	}
	if latest != current.ID {
		return errors.New("abandoned native session was rebound before retirement")
	}
	return nil
}
