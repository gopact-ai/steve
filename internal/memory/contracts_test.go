package memory

// The only production implementation of capabilities memory probes for.
var (
	_ auditSink   = (*LedgerStore)(nil)
	_ readChecker = (*LedgerStore)(nil)
)
