package app

import "time"

func assembleRecovery(input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, work executionAssembly, page consoleAssembly) error {
	// Load the delivery owner before projecting any task accounting. A failed
	// acceptance must leave the original open row discoverable on the next pass.
	if err := page.Console().PersistLedger(boot.Book()); err != nil {
		return err
	}
	work.Coordinator().ResumePlans(boot.Context())
	recovery := newApplicationRecovery(boot.Book(), storage.Attempts(), work.Tasks(), work.Coordinator(), work.Gateway(), page.Console(), work.CatalogText(), input.Environment() != nil)
	if err := recovery.reconcile(boot.Context(), false); err != nil {
		return err
	}
	if workers := page.Reconciliations(); workers != nil {
		recovery.workers = workers
		workers.Go(func() {
			runReconciler(boot.Context(), "reconcile chat completion and continuation", recovery.Reconcile)
		})
	}
	return nil
}

// staleTask governs whether a new continuation is welcome, never retention of
// execution evidence. Old accounting/result receipts remain recoverable.
const staleTask = 24 * time.Hour
