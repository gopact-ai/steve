package httpapi

import "github.com/gopact-ai/steve/internal/console"

// The only production implementations of capabilities httpapi probes for.
var (
	_ localizedVerbs         = (*console.Service)(nil)
	_ submissionCapabilities = (*console.Service)(nil)
)
