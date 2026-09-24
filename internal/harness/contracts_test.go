package harness

// The only production implementation of these runner capabilities.
var (
	_ NativeContextSession     = (*managedSession)(nil)
	_ NodeReceiptSource        = (*managedSession)(nil)
	_ ResumableRunner          = (*managedSession)(nil)
	_ RetainedSessionInspector = (*managedSession)(nil)
	_ RetainedStopper          = (*managedSession)(nil)
)

// The runners Manager opens: local and plugin sessions are *Session, node-owned
// ones *managedSession. Both implement every capability below.
var (
	_ Configurable = (*Session)(nil)
	_ Configurable = (*managedSession)(nil)
	_ Reobserver   = (*Session)(nil)
	_ Reobserver   = (*managedSession)(nil)
	// exec's stepSession, which lifecycle also drives, does not implement
	// TurnRunner on purpose: its Prompt calls AgentRunner.RunStep, which
	// opens one of these runners and chooses between Prompt and PromptTurn.
	_ TurnRunner = (*Session)(nil)
	_ TurnRunner = (*managedSession)(nil)
)
