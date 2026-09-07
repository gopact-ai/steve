package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AcquireLocal joins an existing atomic admission transaction. It has the
// same fencing semantics as Acquire and never contacts a foreign issuer.
func (t *Tx) AcquireLocal(key, holder string, ttl time.Duration) (Lease, error) {
	if key == "" || holder == "" || ttl <= 0 {
		return Lease{}, errors.New("lease key, holder and ttl are required")
	}
	var epoch uint64
	var current, expires string
	err := t.QueryRow(`SELECT epoch, holder, expires_at FROM leases WHERE resource_key = ?`, key).Scan(&epoch, &current, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Lease{}, err
	}
	if current != "" && current != holder {
		until, _ := time.Parse(time.RFC3339Nano, expires)
		if until.After(t.l.now()) {
			return Lease{}, fmt.Errorf("%w: %s is held by %s", ErrHeld, key, current)
		}
	}
	if epoch == ^uint64(0) {
		return Lease{}, errors.New("lease epoch exhausted")
	}
	lease := Lease{Region: t.l.Region(), Key: key, Holder: holder, Epoch: epoch + 1, Incarnation: t.l.Incarnation(), ExpiresAt: t.l.now().Add(ttl).UTC()}
	_, err = t.Exec(`INSERT INTO leases(resource_key, incarnation, epoch, holder, expires_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(resource_key) DO UPDATE SET incarnation = excluded.incarnation, epoch = excluded.epoch, holder = excluded.holder, expires_at = excluded.expires_at`, key, lease.Incarnation, lease.Epoch, holder, lease.ExpiresAt.Format(time.RFC3339Nano))
	return lease, err
}

// RetireStopped fences only exact leases of a positively stopped execution.
// Resources already acquired by another execution remain untouched.
func (t *Tx) RetireStopped(leases []Lease) error {
	for _, lease := range leases {
		if t.l.foreign(lease) {
			return fmt.Errorf("%w: recovery requires the original lease issuer", ErrUnknownRegion)
		}
		if lease.Epoch == ^uint64(0) {
			return errors.New("lease epoch exhausted")
		}
		if _, err := t.Exec(`UPDATE leases SET epoch = ?, holder = '', expires_at = ? WHERE resource_key = ? AND incarnation = ? AND epoch = ? AND holder = ?`, lease.Epoch+1, t.l.now().UTC().Format(time.RFC3339Nano), lease.Key, lease.Incarnation, lease.Epoch, lease.Holder); err != nil {
			return err
		}
	}
	return nil
}
