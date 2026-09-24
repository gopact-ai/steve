package user

import (
	"io"

	ports "github.com/gopact-ai/steve/internal/port"
)

type reader interface{ Read() string }

func Use(v any) {
	_, _ = v.(ports.Closer)
	_, _ = v.(io.Closer)
	switch v.(type) {
	case ports.Closer, reader:
	case interface{ Other() }:
	case nil:
	}
}
