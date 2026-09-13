//go:build !unix

package nativehistory

import (
	"context"
	"errors"
)

// Native history execution currently requires POSIX node path semantics and
// a process-safe storage lock. Do not claim the bound on unsupported systems.
func LockStorage(context.Context, string) (func(), error) {
	return nil, errors.New("native history storage locking requires a POSIX node")
}
