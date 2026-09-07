package agentexec

import (
	"context"

	"github.com/gopact-ai/steve/internal/view"
)

type progressKey struct{}

// WithProgress keeps streamed planning/step/verification updates attached to
// the adapter that owns this invocation, including a recovered exchange.
func WithProgress(ctx context.Context, observe func(view.Progress)) context.Context {
	if observe == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, observe)
}

func EmitProgress(ctx context.Context, progress view.Progress) {
	if observe, ok := ctx.Value(progressKey{}).(func(view.Progress)); ok {
		observe(progress)
	}
}
