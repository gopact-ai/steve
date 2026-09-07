package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNodeCanStartWithoutRegisteredAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte(`{"name":"storage-node","listen":"127.0.0.1:0","token":"test-token","harnesses":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Harnesses) != 0 {
		t.Fatalf("unexpected implicit harnesses: %+v", cfg.Harnesses)
	}
	if err := prepareAdapters(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
}
