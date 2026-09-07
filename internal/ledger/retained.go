package ledger

import (
	"errors"
	"fmt"
	"time"
)

// RenewRetained extends exactly the same local lease tuples after their
// execution owner has positively verified a retained node session. Expiry
// alone does not change the holder. A moved epoch, incarnation, holder or
// foreign issuer is always refused. The caller checks task authorization in
// this same transaction; this operation cannot admit a replacement process.
func (t *Tx) RenewRetained(leases []Lease, ttl time.Duration) ([]Lease, error) {
	if len(leases) == 0 || ttl <= 0 {
		return nil, errors.New("retained lease recovery requires leases and a positive duration")
	}
	updated := make([]Lease, 0, len(leases))
	for _, lease := range leases {
		if lease.Key == "" || lease.Holder == "" || lease.Incarnation != t.l.Incarnation() || t.l.foreign(lease) {
			return nil, fmt.Errorf("%w: retained lease identity differs", ErrStale)
		}
		var incarnation, epoch uint64
		var holder string
		if err := t.QueryRow(`SELECT incarnation, epoch, holder FROM leases WHERE resource_key = ?`, lease.Key).Scan(&incarnation, &epoch, &holder); err != nil {
			return nil, err
		}
		if incarnation != lease.Incarnation || epoch != lease.Epoch || holder != lease.Holder {
			return nil, fmt.Errorf("%w: retained lease %s changed holder or generation", ErrStale, lease.Key)
		}
		lease.ExpiresAt = t.l.now().Add(ttl).UTC()
		if _, err := t.Exec(`UPDATE leases SET expires_at = ? WHERE resource_key = ?`, lease.ExpiresAt.Format(time.RFC3339Nano), lease.Key); err != nil {
			return nil, err
		}
		updated = append(updated, lease)
	}
	return updated, nil
}
