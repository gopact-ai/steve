package platformconfig

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
)

func classifiedConfig(level string) *config.Config {
	return &config.Config{Gateway: config.Gateway{OwnerID: "owner", HomePath: "/private/home"}, Projects: map[string]config.Project{"work": {Level: level, Home: config.ProjectHome{Path: "/private/work"}}}}
}

func classificationNode() LocalNode {
	return LocalNode{ID: "machine", Config: config.Node{Addr: "127.0.0.1:7701", Token: "fixture-token", Level: "restricted"}}
}

func TestSharedConfigurationRejectsSealedBeforeBootstrapWrites(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	store := New(book)
	if _, err := store.Bootstrap(t.Context(), classifiedConfig("sealed"), classificationNode()); err == nil {
		t.Fatal("shared ledger accepted sealed project bootstrap")
	}
	if _, ok, err := book.Document(document).Load(); err != nil || ok {
		t.Fatalf("rejected bootstrap wrote shared data: %v %v", ok, err)
	}
	if _, err := FromLocal(classifiedConfig("sealed"), classificationNode()); err == nil {
		t.Fatal("sealed local config converted to shared declaration")
	}
}

func TestSharedConfigurationRejectsSealedCandidateAndPreservesRevision(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	store := New(book)
	first, err := store.Bootstrap(t.Context(), classifiedConfig("restricted"), classificationNode())
	if err != nil {
		t.Fatal(err)
	}
	candidate := classifiedConfig("sealed")
	candidate.Nodes = first.Nodes
	candidate.Projects["work"] = config.Project{Level: "sealed", Home: config.ProjectHome{Node: "machine", Path: "/private/work"}}
	if _, err := first.WithCandidate(candidate); err == nil {
		t.Fatal("sealed edit converted to shared candidate")
	}
	blocked := first
	blocked.Projects = map[string]config.Project{"work": candidate.Projects["work"]}
	if _, err := store.Save(t.Context(), first.Revision, blocked); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("sealed save did not report data boundary: %v", err)
	}
	current, ok, err := store.Load()
	if err != nil || !ok || current.Revision != first.Revision || current.Projects["work"].Level != "restricted" {
		t.Fatalf("rejected save changed shared config: %+v %v", current, err)
	}
	if _, err := store.Bootstrap(t.Context(), classifiedConfig("sealed"), classificationNode()); err == nil {
		t.Fatal("existing shared state bypassed local sealed bootstrap check")
	}
}
