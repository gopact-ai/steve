package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Issuer signs leases for one region. The local ledger is the issuer of
// its own region; another region's hub is reached through an Issuer that
// speaks to it. A lease is only ever verified by the region that issued
// it: nobody else can say whether it still holds.
type Issuer interface {
	Acquire(ctx context.Context, key, holder string, ttl time.Duration) (Lease, error)
	Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error)
	Release(ctx context.Context, lease Lease) error
	Check(ctx context.Context, lease Lease) error
	Invalidate(ctx context.Context, key string) error
}

// DefaultRegion is the region of a hub that never named one.
const DefaultRegion = "default"

// ErrUnknownRegion is a lease for a region no issuer is registered for.
var ErrUnknownRegion = errors.New("ledger: unknown region")

var regionMu sync.RWMutex

// SetRegion names the region this ledger issues for.
func (l *Ledger) SetRegion(region string) {
	regionMu.Lock()
	defer regionMu.Unlock()
	l.region = region
}

// Region is this ledger's own region.
func (l *Ledger) Region() string {
	regionMu.RLock()
	defer regionMu.RUnlock()
	if l.region == "" {
		return DefaultRegion
	}
	return l.region
}

// RegisterIssuer wires the issuer of another region.
func (l *Ledger) RegisterIssuer(region string, issuer Issuer) {
	regionMu.Lock()
	defer regionMu.Unlock()
	if l.issuers == nil {
		l.issuers = map[string]Issuer{}
	}
	l.issuers[region] = issuer
}

func (l *Ledger) issuerFor(region string) (Issuer, bool) {
	if region == "" || region == l.Region() {
		return nil, true
	}
	regionMu.RLock()
	defer regionMu.RUnlock()
	issuer, ok := l.issuers[region]
	return issuer, ok
}

// AcquireIn takes a lease from the region's issuer.
func (l *Ledger) AcquireIn(ctx context.Context, region, key, holder string, ttl time.Duration) (Lease, error) {
	issuer, ok := l.issuerFor(region)
	if !ok {
		return Lease{}, fmt.Errorf("%w: %s", ErrUnknownRegion, region)
	}
	if issuer == nil {
		lease, err := l.Acquire(ctx, key, holder, ttl)
		lease.Region = l.Region()
		return lease, err
	}
	lease, err := issuer.Acquire(ctx, key, holder, ttl)
	if err != nil {
		return Lease{}, err
	}
	lease.Region = region
	return lease, nil
}

// RenewAny renews a lease with whichever region issued it.
func (l *Ledger) RenewAny(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	issuer, ok := l.issuerFor(lease.Region)
	if !ok {
		return Lease{}, fmt.Errorf("%w: %s", ErrUnknownRegion, lease.Region)
	}
	if issuer == nil {
		next, err := l.Renew(ctx, lease, ttl)
		next.Region = lease.Region
		return next, err
	}
	next, err := issuer.Renew(ctx, lease, ttl)
	next.Region = lease.Region
	return next, err
}

// ReleaseAny releases a lease with whichever region issued it.
func (l *Ledger) ReleaseAny(ctx context.Context, lease Lease) error {
	issuer, ok := l.issuerFor(lease.Region)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownRegion, lease.Region)
	}
	if issuer == nil {
		return l.Release(ctx, lease)
	}
	return issuer.Release(ctx, lease)
}

// Check verifies a lease of this region; it is what other regions call.
func (l *Ledger) Check(ctx context.Context, lease Lease) error {
	tx, err := l.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return l.checkLease(ctx, tx, lease)
}

// checkForeign verifies fencings issued elsewhere before a local
// transition. A foreign lease can still lapse between this check and the
// commit — that window is the price of a second region, and why nothing a
// foreign lease guards is bound without a receipt from its region.
func (l *Ledger) checkForeign(ctx context.Context, fencings []Lease) error {
	for _, lease := range fencings {
		issuer, ok := l.issuerFor(lease.Region)
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownRegion, lease.Region)
		}
		if issuer == nil {
			continue
		}
		if err := issuer.Check(ctx, lease); err != nil {
			return err
		}
	}
	return nil
}

func (l *Ledger) foreign(lease Lease) bool {
	return lease.Region != "" && lease.Region != l.Region()
}

// InvalidateIn forces a resource's epoch forward in the region that
// issues it.
func (l *Ledger) InvalidateIn(ctx context.Context, region, key string) error {
	issuer, ok := l.issuerFor(region)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownRegion, region)
	}
	if issuer == nil {
		return l.Invalidate(ctx, key)
	}
	return issuer.Invalidate(ctx, key)
}
