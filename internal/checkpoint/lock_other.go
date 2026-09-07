//go:build !unix && !windows

package checkpoint

import (
	"errors"
	"os"
)

func lockStore(*os.File) error {
	return errors.New("checkpoint storage locking is unsupported on this platform")
}
