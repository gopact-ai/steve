package memory

// Bind the optional sides the two stores are meant to have, so renaming or
// resigning one of these methods fails to compile instead of quietly
// dropping the Service back to the required method: a store that stops
// reporting Text serves cut snapshots to the profile page, and one that
// stops keeping its own audit writes the Service's file instead. The
// absences are as deliberate as the bindings. Markdown has no authority to
// check a read against and no ledger to audit into, and Recall reports a
// store that does not name itself as "markdown"; the ledger store keeps no
// scope in a file it can name.
var (
	_ textReader   = (*LedgerStore)(nil)
	_ readChecker  = (*LedgerStore)(nil)
	_ namedSource  = (*LedgerStore)(nil)
	_ auditSink    = (*LedgerStore)(nil)
	_ textReader   = (*Markdown)(nil)
	_ pathReporter = (*Markdown)(nil)
)
