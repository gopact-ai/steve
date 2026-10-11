package work

// States are shared by persisted records and their guarded transitions.
const (
	StatePending  = "pending"
	StateAccepted = "accepted"
	StateFailed   = "failed"
)
