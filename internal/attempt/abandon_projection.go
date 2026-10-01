package attempt

import (
	"context"
	"errors"
)

// ProjectAbandonedCapacity gives up only endpoint capacity, not writer leases.
// The caller must first durably project the exact session retirement.
func (s *Service) ProjectAbandonedCapacity(ctx context.Context, id string, revision uint64) (Record, error) {
	return Record{}, errors.New("abandoned capacity projection is not available")
}
