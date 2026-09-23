package view

const (
	// PhaseFinishing begins after the agent has returned, while execution
	// results and workspace changes are being finalized.
	PhaseFinishing Phase = "finishing"
	// PhaseSaving keeps the turn visibly active until its reply is durable.
	PhaseSaving Phase = "saving"
)

// Stage names the platform's own preparation step inside the waking
// phase. "Preparing" for twenty seconds says nothing about whether a
// workspace is being synced or a cold agent is starting, so each step
// that can take real time reports itself and the surface says which one
// is running. Stages are codes, not sentences: every surface writes them
// in the reader's own language.
type Stage string

const (
	// StageWorkspace is settling the directory the turn runs in, which on
	// a fresh project or a remote node includes creating or syncing it.
	StageWorkspace Stage = "workspace"
	// StageCapabilities is assembling identity, skills and MCP servers.
	StageCapabilities Stage = "capabilities"
	// StagePlacement is leasing the attempt and asking the machine whether
	// it can run this agent.
	StagePlacement Stage = "placement"
	// StageAwaitSnapshot is waiting for a snapshot of the project's
	// canonical workspace, cut for other work, to give the workspace back.
	StageAwaitSnapshot Stage = "await-snapshot"
	// StageSnapshot is recording what the workspace looked like before the
	// turn, the baseline its changes are measured against.
	StageSnapshot Stage = "snapshot"
	// StageSession is opening the agent session: the cold start.
	StageSession Stage = "session"
	// StageResume is reopening the session this conversation already has.
	StageResume Stage = "resume"
)
