package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// Every first-party package in plugins/ must be importable as shipped.
func TestFirstPartyPackagesAreValid(t *testing.T) {
	root := filepath.Join("..", "..", "plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		found++
		bundle, err := ReadDirectory(t.Context(), filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if bundle.Manifest.ID != "gopact/"+entry.Name() {
			t.Fatalf("%s declares id %q; first-party packages use gopact/<directory>", entry.Name(), bundle.Manifest.ID)
		}
	}
	if found == 0 {
		t.Fatal("no first-party packages")
	}
}
