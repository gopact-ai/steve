package user

import ports "github.com/gopact-ai/steve/internal/port"

// fake is declared in a test file, so it pins no production type.
type fake struct{}

var (
	_ ports.Closer   = (*closer)(nil)
	_ ports.Recorder = ports.Record{}
	_ ports.Faked    = fake{}
	_ ports.Nilled   = nil
	_                = 1
)
