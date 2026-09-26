package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type uncheckablePolicy struct{}

func (uncheckablePolicy) CheckpointPlacement(context.Context, Scope, string) (Placement, error) {
	return Placement{}, fmt.Errorf("%w: committed state out of reach", ErrUnavailable)
}

// A placement the policy could not check for now refuses nothing: the store
// reports the check as unavailable, not the placement as refused, so its
// caller asks again instead of giving the content up.
func TestAPlacementThatCouldNotBeCheckedIsNotRefused(t *testing.T) {
	store := newTestStore(t, "node-a", nil, nil, uncheckablePolicy{}, Limits{})
	scope := Scope{ProjectID: "project-1", Level: "internal", HomeNodeID: "node-a"}
	_, err := store.HasBlob(t.Context(), scope, Reference([]byte("asked again later")))
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrPlacement) {
		t.Fatalf("unchecked placement: %v, want unavailable and not refused", err)
	}
}
