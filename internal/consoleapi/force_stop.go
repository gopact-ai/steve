package consoleapi

import "context"

// ForceStops records owner requests to stop an original native execution.
type ForceStops interface {
	ForceStop(context.Context, string) error
}
