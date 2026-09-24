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

// Named types of this package that hold synchronization state, directly
// or through another such type, in this file or in another.
var (
	held      holder
	deep      *nested
	boxed     box[int]
	literal   = holder{}
	allocated = new(nested)
)

type nested struct {
	inner holder
}

type box[T any] struct {
	value T
	mu    a.Int32
}

type plain struct{ n int }

// Not synchronization state.
var (
	calm     plain
	fielded  struct{ holder int }
	selected = names["holder"]
	names    = map[string]int{}
	helper   = func() { var local sync.Mutex; local.Lock() }
	limit    = 3
	counted  func() a.Int64
	_        sync.Locker = (*sync.Mutex)(nil)
)

type holder struct{ mu sync.Mutex }

func function() {
	var local sync.Mutex
	local.Lock()
}
