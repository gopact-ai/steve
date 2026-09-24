package user

import ports "github.com/gopact-ai/steve/internal/port"

var (
	_ ports.Closer = closer{}
	_              = 1
)
