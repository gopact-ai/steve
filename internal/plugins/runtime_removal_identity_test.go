package plugins

import (
	"errors"
	"strings"
	"testing"
)

func TestRuntimeRemovalPreflightRejectsDifferentSelection(t *testing.T) {
	store, record := runtimeUsageFixture(t)
	if err := store.RetireRuntime(t.Context(), record.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckRuntimeRemovable(record.Ref); err != nil {
		t.Fatalf("original retired unused runtime must remain removable: %v", err)
	}
	for _, field := range []string{"project", "node", "harness", "deployment", "excluded-skill"} {
		t.Run(field, func(t *testing.T) {
			drift := record.Ref.Clone()
			switch field {
			case "project":
				drift.Selection.Project = "other-project"
			case "node":
				drift.Selection.Node = "other-worker"
			case "harness":
				drift.Selection.Harness = "other-harness"
			case "deployment":
				drift.Selection.Deployments = []string{strings.Repeat("f", 64)}
			case "excluded-skill":
				drift.Selection.ExcludedSkills = []string{"other-skill"}
			}
			if err := drift.Validate(); err != nil {
				t.Fatalf("requires a valid caller reference, not malformed input: %v", err)
			}
			// The preflight is used before a caller drops its live broker.
			// A later RemoveRuntime refusal cannot undo that side effect.
			if err := store.CheckRuntimeRemovable(*drift); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("different %s granted runtime-removal permission: %v", field, err)
			}
			if err := store.CheckRuntimeRemovable(record.Ref); err != nil {
				t.Fatalf("rejected caller changed original removability: %v", err)
			}
		})
	}
}
