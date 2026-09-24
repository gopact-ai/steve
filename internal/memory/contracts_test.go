package memory

// The only production implementation of capabilities memory probes for.
var (
	_ auditSink   = (*LedgerStore)(nil)
	_ readChecker = (*LedgerStore)(nil)
)

// The only production implementation of pathReporter: the Markdown store
// keeps each scope in a file it can name; the ledger store has no file.
var _ pathReporter = (*Markdown)(nil)
