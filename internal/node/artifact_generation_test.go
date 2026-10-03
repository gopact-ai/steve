package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestLocalArtifactEndpointSharesItsGenerationContract(t *testing.T) {
	r := NewRegistry("hub", nil)
	t.Cleanup(r.Close)
	generation, err := r.Generation(t.Context(), "")
	if err != nil || generation != 1 {
		t.Fatalf("local artifact endpoint has no fixed generation: %d %v", generation, err)
	}
	dir := filepath.Join(t.TempDir(), "objects.git")
	if _, err := r.Artifact(t.Context(), "", ops.Request{Op: ops.Init, Repo: dir, Generation: generation}); err != nil {
		t.Fatalf("local generation could not dispatch its exact request: %v", err)
	}
	if _, err := r.Artifact(t.Context(), "", ops.Request{Op: ops.Init, Repo: filepath.Join(t.TempDir(), "refused.git"), Generation: generation + 1}); err == nil {
		t.Fatal("another local generation acquired execution permission")
	}
	if value, err := r.Generation(t.Context(), "unknown"); err == nil || value != 0 {
		t.Fatalf("unknown remote inherited the local generation: %d %v", value, err)
	}
}

func TestArtifactDispatchUsesTheSelectedConnectionGeneration(t *testing.T) {
	registry, _ := artifactNode(t)
	c, err := registry.connect(t.Context(), "n")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "objects.git")
	raw, err := json.Marshal(map[string]any{"op": ops.Init, "repo": dir, "generation": c.generation + 1})
	if err != nil {
		t.Fatal(err)
	}
	var req nodewire.ArtifactRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Artifact(t.Context(), "n", req); err == nil {
		t.Fatal("operation was sent on a different generation")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("refused connection dispatch performed filesystem I/O: %v", err)
	}
	raw, err = json.Marshal(map[string]any{"op": ops.Init, "repo": dir, "generation": c.generation})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Artifact(t.Context(), "n", req); err != nil {
		t.Fatalf("exact connection generation refused: %v", err)
	}
}
