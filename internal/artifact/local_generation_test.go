package artifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/artifact/ops"
)

func TestLocalArtifactDispatchRefusesAnotherGeneration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "objects.git")
	raw, err := json.Marshal(map[string]any{"op": ops.Init, "repo": dir, "generation": 2})
	if err != nil {
		t.Fatal(err)
	}
	var req ops.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if _, err := (LocalNodes{}).Artifact(t.Context(), "", req); err == nil {
		t.Fatal("local execution accepted another generation")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("refused dispatch performed filesystem I/O: %v", err)
	}
}
