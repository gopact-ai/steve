package readmodel

// ExecutionState is the observed activity rolled up over a task and its children.
// It is independent of the task's durable lifecycle.
type ExecutionState string

const (
	ExecutionIdle    ExecutionState = "idle"
	ExecutionRunning ExecutionState = "running"
	ExecutionUnknown ExecutionState = "unknown"
)
