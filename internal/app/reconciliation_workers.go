package app

import "sync"

// reconciliationWorkers closes admission before joining work. A dispatcher
// racing shutdown cannot add a new worker after Wait observes an empty group.
// Execution permission and durable recovery evidence remain with their owners.
type reconciliationWorkers struct {
	mu      sync.Mutex
	closing bool
	group   sync.WaitGroup
}

func (w *reconciliationWorkers) Go(run func()) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing {
		return false
	}
	w.group.Go(run)
	return true
}

func (w *reconciliationWorkers) Close() {
	w.mu.Lock()
	w.closing = true
	w.mu.Unlock()
	w.group.Wait()
}
