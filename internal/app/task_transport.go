package app

import "fmt"

// routeTask selects only an explicitly persisted transport. Native chat and
// message IDs are opaque adapter data, not evidence of a channel's identity.
func routeTask(transport string, page, chat func() error) error {
	switch transport {
	case "console":
		return page()
	case "feishu":
		return chat()
	default:
		return fmt.Errorf("unsupported task transport %q", transport)
	}
}
