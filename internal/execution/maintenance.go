package execution

import (
	"errors"
	"sort"
)

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

// Active names the executions that are keeping the registry busy, most
// specific identifier first. A restart that cannot start reports them so
// the person can see which work to finish or stop, rather than being told
// only that something is active.
func (r *Registry) Active() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for scope := range r.entries {
		name := scope.key.InstanceID
		if name == "" {
			name = scope.key.AttemptID
		}
		if name == "" {
			name = scope.key.TaskID
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
