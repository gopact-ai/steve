package consoleapi

type ExchangeState string

const (
	ExchangeQueued       ExchangeState = "queued"
	ExchangeRunning      ExchangeState = "running"
	ExchangeRecovering   ExchangeState = "recovering"
	ExchangeAwaitingUser ExchangeState = "awaiting-user"
	ExchangeDone         ExchangeState = "done"
	ExchangeFailed       ExchangeState = "failed"
	ExchangeCancelled    ExchangeState = "cancelled"
)

// Terminal identifies a submission with a final receipt, including failure.
// A recovering or awaiting-user exchange still owns its queue reservation.
func (s ExchangeState) Terminal() bool {
	return s == ExchangeDone || s == ExchangeFailed || s == ExchangeCancelled
}
