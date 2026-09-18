//go:build !unix

package processrestart

import "errors"

func Supported() bool      { return false }
func ReexecCurrent() error { return Reexec("") }

func Reexec(string) error {
	return errors.New("process restart is not supported on this platform")
}
