package execution

import "errors"

var ErrBusy = errors.New("executions are active or awaiting stop verification")

// SealIdle closes admission atomically with checking all execution owners.
// The release function is used only if maintenance is abandoned or finishes.
func (r *Registry) SealIdle() (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || len(r.entries) != 0 {
		return nil, ErrBusy
	}
	r.closing = true
	return func() { r.mu.Lock(); defer r.mu.Unlock(); r.closing = false }, nil
}
