package sample

import (
	"sync"
	a "sync/atomic"
)

var (
	mu      sync.Mutex
	rw      *sync.RWMutex
	value   a.Value
	counter a.Int64
	once    = sync.OnceValue(func() int { return 1 })
	shared  = &sync.Map{}
	made    = new(sync.WaitGroup)
	guarded struct {
		sync.Mutex
		n int
	}
	pointer a.Pointer[int]
)

// Not synchronization state.
var (
	names   = map[string]int{}
	helper  = func() { var local sync.Mutex; local.Lock() }
	limit   = 3
	counted func() a.Int64
	_       sync.Locker = (*sync.Mutex)(nil)
)

type holder struct{ mu sync.Mutex }

func function() {
	var local sync.Mutex
	local.Lock()
}
