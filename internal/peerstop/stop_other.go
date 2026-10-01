//go:build !linux

package peerstop

import "context"

func stopInstallation(context.Context, string, string, string) (bool, error) {
	return false, ErrUnsupported
}
