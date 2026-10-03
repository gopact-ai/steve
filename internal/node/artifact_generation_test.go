package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/nodewire"
)

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
