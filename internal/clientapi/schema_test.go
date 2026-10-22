package clientapi

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The committed schema is what clients generate their types from, so it
// must describe the types this server actually encodes. Regenerate it with
// UPDATE_SCHEMA=1 go test ./internal/clientapi -run TestCommittedSchemaIsCurrent.
func TestCommittedSchemaIsCurrent(t *testing.T) {
	want, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", filepath.FromSlash(SchemaPath))
	if os.Getenv("UPDATE_SCHEMA") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v; generate it with UPDATE_SCHEMA=1", SchemaPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate it with UPDATE_SCHEMA=1 go test ./internal/clientapi -run TestCommittedSchemaIsCurrent", SchemaPath)
	}
}
