package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/project"
)

func materialsAdmin(t *testing.T, level project.Level) (*fleetAdmin, *ledger.Ledger) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	materials, err := material.Open(t.TempDir(), book)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { materials.Close() })
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}, Level: level}}); err != nil {
		t.Fatal(err)
	}
	return &fleetAdmin{cfg: &config.Config{Gateway: config.Gateway{OwnerID: "owner"}}, owner: "owner", materialLevel: project.LevelRestricted, materials: materials, projects: projects}, book
}
func TestMaterialsDoNotCrossHubDataLevelOrPendingOwnerChange(t *testing.T) {
	a, _ := materialsAdmin(t, project.LevelRestricted)
	a.materialLevel = project.LevelPublic
	if _, err := a.UploadMaterial(t.Context(), "p", "secret.txt", "text/plain", strings.NewReader("secret")); !errors.Is(err, material.ErrScope) {
		t.Fatalf("public Hub received restricted content: %v", err)
	}
	a.materialLevel = project.LevelRestricted
	item, err := a.UploadMaterial(t.Context(), "p", "note.txt", "text/plain", strings.NewReader("note"))
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.Gateway.OwnerID = "pending-new-owner"
	annotation, err := a.SaveMaterialAnnotation(t.Context(), material.AnnotationInput{ID: "mark", Project: "p", Ref: material.Ref{ID: item.ID}, Body: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if annotation.Author != "owner" {
		t.Fatal("pending owner became active before restart")
	}
}
func TestReplyCaptureUsesOriginalProjectAndRevision(t *testing.T) {
	a, book := materialsAdmin(t, project.LevelPublic)
	replies := map[string]any{"replies": map[string][]consoleapi.Reply{"console:one": {{ID: "reply", Conversation: "console:one", ProjectID: "p", Text: "original", Kind: "reply"}}}}
	data, _ := json.Marshal(replies)
	if err := book.Document("console").Save(data); err != nil {
		t.Fatal(err)
	}
	a.console = console.New(nil, "owner", nil)
	if err := a.console.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	reply := a.console.Replies("console:one")[0]
	req := consoleapi.MaterialCapture{Project: "p", Source: material.Source{Kind: "reply", Conversation: reply.Conversation, ReplyID: reply.ID, Revision: reply.Revision}}
	capture, err := a.CaptureMaterial(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := a.MaterialContent(t.Context(), "p", capture.ID)
	if err != nil || string(raw) != "original" {
		t.Fatal(string(raw), err)
	}
	req.Source.Revision = "changed"
	if _, err := a.CaptureMaterial(t.Context(), req); !errors.Is(err, material.ErrConflict) {
		t.Fatalf("captured a different version: %v", err)
	}
}
