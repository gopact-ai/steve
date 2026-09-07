package ledger

import (
	"context"
	"fmt"
)

// CheckLocalLease validates a locally issued lease within the caller's
// transaction. Foreign lease checks cannot be made atomic with this ledger.
func (t *Tx) CheckLocalLease(lease Lease) error {
	if t.l.foreign(lease) {
		return fmt.Errorf("%w: lease %s is not issued by this ledger", ErrUnknownRegion, lease.Key)
	}
	return t.l.checkLease(t.ctx, t.tx, lease)
}

// CheckAny verifies the exact lease tuple with its issuing region.
func (l *Ledger) CheckAny(ctx context.Context, lease Lease) error {
	issuer, ok := l.issuerFor(lease.Region)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownRegion, lease.Region)
	}
	if issuer != nil {
		return issuer.Check(ctx, lease)
	}
	return l.Check(ctx, lease)
}
