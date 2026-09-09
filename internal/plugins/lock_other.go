//go:build !unix

package plugins

import (
	"context"
	"fmt"
)

func (s *Store) lock(context.Context) (func(), error) {
	return nil, fmt.Errorf("%w: package installation needs an operating system with file locking", ErrIncompatible)
}
