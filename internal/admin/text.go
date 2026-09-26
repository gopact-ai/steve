package admin

import (
	"context"

	"github.com/gopact-ai/steve/internal/i18n"
)

// textFor is the catalog in the language of the person a request comes
// from: the console's Accept-Language, or the language a turn or the
// agent tool server put on the context.
func textFor(ctx context.Context) i18n.Catalog {
	return i18n.FromContext(ctx)
}

// saidError is a message in the reader's language standing for a sentinel
// callers match with errors.Is; the sentinel's own text is never shown.
type saidError struct {
	message  string
	sentinel error
}

func (e saidError) Error() string { return e.message }
func (e saidError) Unwrap() error { return e.sentinel }
