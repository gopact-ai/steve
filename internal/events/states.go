package events

// States are shared by persisted records and their guarded transitions.
const (
	StatePending   = "pending"
	StateClaimed   = "claimed"
	StateAccepted  = "accepted"
	StateDone      = "done"
	StateFailed    = "failed"
	StateUnknown   = "unknown"
	StateCancelled = "cancelled"
	StateOpen      = "open"
	StateResolved  = "resolved"
)
