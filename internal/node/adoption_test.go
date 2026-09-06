package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/runtime"
)

func TestOfflineAdoptionRequiresExitEvidenceAndPersistsStatement(t *testing.T) {
	state := t.TempDir()
	stream := filepath.Join(state, "streams", "old")
	if err := os.MkdirAll(stream, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Adopt(state, "next"); err == nil {
		t.Fatal("unconfirmed child process adopted")
	}
	if err := AdoptWithEvidence(state, "next", "operator", "verified old process group exited"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(state, "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	var owner hubOwner
	if json.Unmarshal(raw, &owner) != nil || owner.Hub != "next" || !owner.Released {
		t.Fatalf("owner=%s", raw)
	}
	if raw, err := os.ReadFile(filepath.Join(state, "last-adoption.json")); err != nil || len(raw) == 0 {
		t.Fatal("evidence not persisted", err)
	}
	unlock, err := runtime.AcquireLock(filepath.Join(state, "instance-control"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := AdoptWithEvidence(state, "another", "operator", "claimed stopped"); err == nil {
		t.Fatal("live instance adoption accepted")
	}
}
func TestOwnershipPersistenceErrorsAreNotIgnored(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "hub.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Adopt(state, "next"); err == nil {
		t.Fatal("unwritable ownership was reported successful")
	}
}
