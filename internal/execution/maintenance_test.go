package execution

import (
	"context"
	"errors"
	"testing"
)

func TestMaintenanceSealsOnlyIdleExecutionAdmission(t *testing.T) {
	r := New(context.Background(), nil)
	scope, err := r.Begin(t.Context(), Key{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SealIdle(); !errors.Is(err, ErrBusy) {
		t.Fatalf("active seal=%v", err)
	}
	scope.Finish(nil)
	release, err := r.SealIdle()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(t.Context(), Key{}); err == nil {
		t.Fatal("new execution crossed maintenance")
	}
	release()
	next, err := r.Begin(t.Context(), Key{})
	if err != nil {
		t.Fatal(err)
	}
	next.Finish(nil)
}
