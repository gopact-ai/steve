package acphost

import "context"

// SupportsResume reads the running adapter's capability declaration without
// opening any session or submitting a prompt.
func (h *Host) SupportsResume(ctx context.Context) (bool, error) {
	if err := h.ensureStarted(ctx); err != nil {
		return false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	caps := h.capabilities
	return caps != nil && (caps.LoadSession || (caps.SessionCapabilities != nil && caps.SessionCapabilities.Resume != nil)), nil
}
