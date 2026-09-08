package view

const (
	// PhaseFinishing begins after the agent has returned, while execution
	// results and workspace changes are being finalized.
	PhaseFinishing Phase = "finishing"
	// PhaseSaving keeps the turn visibly active until its reply is durable.
	PhaseSaving Phase = "saving"
)
