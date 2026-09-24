package consoleapi

import "io"

type Admin interface {
	Called()
	Extra
	Deeper
	io.Closer
}
