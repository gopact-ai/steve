package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ProjectAbandonedCapacity gives up only endpoint capacity, not writer leases.
// The caller must first durably project the exact session retirement. Each
// release checks the original lease tuple; a later holder is never released.
func (s *Service) ProjectAbandonedCapacity(ctx context.Context, id string, revision uint64) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if current.Abandoned == nil || current.Abandoned.ForceStopRevision != revision {
		return Record{}, ErrForceStopChanged
	}
	if !current.Abandoned.ProjectedAt.IsZero() {
		return current, nil
	}
	var capacity []ledger.Lease
	for _, lease := range current.Leases {
		if strings.HasPrefix(lease.Key, endpointKey(current.Node, current.Harness)+":slot:") {
			if err := s.l.ReleaseAny(ctx, lease); err != nil && !errors.Is(err, ledger.ErrStale) {
				return Record{}, err
			}
			capacity = append(capacity, lease)
		}
	}
	var next Record
	_, err = s.l.Transition(ctx, id, string(current.State), string(current.State), "abandon-projection", nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		if next.Abandoned == nil || next.Abandoned.ForceStopRevision != revision || !next.Abandoned.At.Equal(current.Abandoned.At) {
			return ErrForceStopChanged
		}
		if !next.Abandoned.ProjectedAt.IsZero() {
			return errAbandonProjected
		}
		projected := *next.Abandoned
		projected.ProjectedAt = s.now().UTC()
		next.Abandoned = &projected
		next.Leases = slices.DeleteFunc(slices.Clone(next.Leases), func(lease ledger.Lease) bool { return slices.Contains(capacity, lease) })
		next.Revision = op.Revision + 1
		return setRecordDataTx(tx, op, next)
	})
	if errors.Is(err, errAbandonProjected) {
		return s.Get(ctx, id)
	}
	return next, err
}

var errAbandonProjected = errors.New("abandonment was already projected")
