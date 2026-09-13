package harness

import (
	"context"

	"github.com/gopact-ai/steve/internal/nativehistory"
)

type nativeImportKey struct{}

// WithNativeImport expresses the selected conversation's required provenance.
// Only the committed attempt binder can authorize it in NodeSessionContext.
func WithNativeImport(ctx context.Context, ref *nativehistory.Reference) context.Context {
	return context.WithValue(ctx, nativeImportKey{}, ref.Clone())
}

func requestedNativeImport(ctx context.Context) *nativehistory.Reference {
	ref, _ := ctx.Value(nativeImportKey{}).(*nativehistory.Reference)
	return ref
}
