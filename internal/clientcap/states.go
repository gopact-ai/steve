package clientcap

// States are shared by persisted records and their guarded transitions.
const (
	StateApproval  = "approval"
	StatePending   = "pending"
	StateClaimed   = "claimed"
	StateDone      = "done"
	StateFailed    = "failed"
	StateUnknown   = "unknown"
	StateCancelled = "cancelled"
)
