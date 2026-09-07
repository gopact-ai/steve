//go:build !unix

package desktop

import (
	"context"
	"errors"
)

func lockFile(string) (func(), error) {
	return nil, errors.New("desktop startup is not supported on this operating system")
}

func lockFileContext(context.Context, string) (func(), error) { return lockFile("") }
