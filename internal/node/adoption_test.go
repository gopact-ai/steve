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

func TestNodeOwnerChangesOnlyAfterOfflineAdoption(t *testing.T) {
	state := t.TempDir()
	first := NewServer(ServerConfig{StateDir: state})
	if err := first.claim("hub-1"); err != nil {
		t.Fatal(err)
	}
	first.release("hub-1", true)
	restarted := NewServer(ServerConfig{StateDir: state})
	if err := restarted.claim("hub-2"); err == nil {
		t.Fatal("restarting after clean disconnect granted another hub ownership")
	}
	if owner := restarted.owner(); owner.Hub != "hub-1" || !owner.Released {
		t.Fatalf("refused handshake changed persisted owner: %+v", owner)
	}
	if err := Adopt(state, "hub-2"); err != nil {
		t.Fatalf("explicit offline adoption failed: %v", err)
	}
	adopted := NewServer(ServerConfig{StateDir: state})
	if err := adopted.claim("hub-3"); err == nil {
		t.Fatal("explicit adoption allowed an unnamed third hub")
	}
	if err := adopted.claim("hub-2"); err != nil {
		t.Fatalf("explicitly adopted hub refused: %v", err)
	}
	adopted.release("hub-2", true)
	if err := adopted.claim("hub-1"); err == nil {
		t.Fatal("former hub regained ownership without adoption")
	}
}

func TestNodeRetainsHubWithoutStateDirectory(t *testing.T) {
	server := NewServer(ServerConfig{})
	if err := server.claim("hub-1"); err != nil {
		t.Fatal(err)
	}
	server.release("hub-1", true)
	if err := server.claim("hub-2"); err == nil {
		t.Fatal("clean disconnect changed the running instance's in-memory owner")
	}
	if err := server.claim("hub-1"); err != nil {
		t.Fatalf("same in-memory owner could not reconnect: %v", err)
	}
}
