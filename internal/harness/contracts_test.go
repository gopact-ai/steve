package harness

// The only production implementation of these runner capabilities.
var (
	_ NativeContextSession     = (*managedSession)(nil)
	_ NodeReceiptSource        = (*managedSession)(nil)
	_ ResumableRunner          = (*managedSession)(nil)
	_ RetainedSessionInspector = (*managedSession)(nil)
	_ RetainedStopper          = (*managedSession)(nil)
)
