package pihome

import "sync"

var locks sync.Map

func lockFor(dir string) *sync.Mutex {
	mu, _ := locks.LoadOrStore(dir, &sync.Mutex{})
	return mu.(*sync.Mutex)
}
