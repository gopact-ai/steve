package contentreplica

import (
	"context"
	"fmt"
	"slices"

	"github.com/gopact-ai/steve/internal/ledger"
)

// Closure resolves immutable bundle dependencies, base first. Missing or
// malformed dependencies are errors, even if a caller has a warm Git cache.
func Closure(ctx context.Context, book *ledger.Ledger, id string) ([]Manifest, error) {
	if !digest(id, 64) {
		return nil, ErrInvalid
	}
	return closure(id, func(id string) (Manifest, bool, error) { return Lookup(ctx, book, id) })
}

func closure(id string, load func(string) (Manifest, bool, error)) ([]Manifest, error) {
	var out []Manifest
	seen := map[string]bool{}
	for id != "" {
		if seen[id] || len(out) == MaxBundleDepth {
			return nil, fmt.Errorf("%w: content dependency cycle or depth at %s", ErrIntegrity, id)
		}
		seen[id] = true
		m, ok, err := load(id)
		if err != nil {
			return nil, fmt.Errorf("content dependency %s: %w", id, err)
		}
		if !ok {
			return nil, fmt.Errorf("%w: content dependency %s", ErrIncomplete, id)
		}
		if len(out) > 0 {
			child := out[len(out)-1]
			if m.Object.Scope != child.Object.Scope || m.Object.Kind != GitBundle || m.Object.Key == child.Object.Key {
				return nil, fmt.Errorf("%w: content dependency scope or commit at %s", ErrIntegrity, id)
			}
		}
		out = append(out, m)
		id = m.Object.Base
	}
	slices.Reverse(out)
	return out, nil
}
