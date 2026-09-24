package user

import (
	"io"

	ports "github.com/gopact-ai/steve/internal/port"
)

type reader interface{ Read() string }

type closer struct{}

func Use(v any) {
	_, _ = v.(ports.Closer)
	_, _ = v.(ports.Recorder)
	_, _ = v.(ports.Faked)
	_, _ = v.(ports.Nilled)
	_, _ = v.(io.Closer)
	switch v.(type) {
	case ports.Closer, reader:
	case interface{ Other() }:
	case nil:
	}
}
