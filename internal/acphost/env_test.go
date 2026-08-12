package acphost

import (
	"slices"
	"testing"
)

func TestMergeEnvOverridesInheritedVariables(t *testing.T) {
	got := mergeEnv(
		[]string{"A=1", "B=2", "PATH=/usr/bin"},
		[]string{"B=9", "PATH=/opt/bin"},
	)
	want := []string{"A=1", "B=9", "PATH=/opt/bin"}
	if !slices.Equal(got, want) {
		t.Fatalf("mergeEnv = %v, want %v", got, want)
	}
}

func TestMergeEnvWithoutOverrides(t *testing.T) {
	base := []string{"A=1"}
	if got := mergeEnv(base, nil); !slices.Equal(got, base) {
		t.Fatalf("mergeEnv = %v, want %v", got, base)
	}
}
