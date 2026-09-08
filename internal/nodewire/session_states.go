package nodewire

// SessionStatus describes the native session, independently of its commands.
type SessionStatus string

const (
	SessionOpening     SessionStatus = "opening"
	SessionIdle        SessionStatus = "idle"
	SessionRunning     SessionStatus = "running"
	SessionConfiguring SessionStatus = "configuring"
	SessionClosing     SessionStatus = "closing"
	SessionClosed      SessionStatus = "closed"
	SessionInterrupted SessionStatus = "interrupted"
)

// Unavailable reports an explicit closed or interrupted native session receipt.
// It does not infer availability from process-stop or command-settlement evidence.
func (s SessionStatus) Unavailable() bool {
	return s == SessionInterrupted || s == SessionClosed
}

type SessionCommandState string

const (
	SessionCommandAccepted  SessionCommandState = "accepted"
	SessionCommandRunning   SessionCommandState = "running"
	SessionCommandCompleted SessionCommandState = "completed"
	SessionCommandCancelled SessionCommandState = "cancelled"
	SessionCommandUncertain SessionCommandState = "uncertain"
)

// Active includes accepted commands whose native dispatch has not started yet.
func (s SessionCommandState) Active() bool {
	return s == SessionCommandAccepted || s == SessionCommandRunning
}
