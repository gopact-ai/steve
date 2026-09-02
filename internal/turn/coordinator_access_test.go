package turn

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
)

// A restricted project admits nobody by default; a grant opens it; the
// owner is admin everywhere and is the only one who can grant.
func TestAccessIsGrantedNotAssumed(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	coordinator.SetIdentity("ou_owner", home.Dir{Path: t.TempDir()})
	ctx := context.Background()
	if err := coordinator.projects.Declare(ctx, []project.Project{{ID: "codex", Level: project.LevelRestricted, Home: project.Home{Path: workspaceOf(t, coordinator, "codex")}}}); err != nil {
		t.Fatal(err)
	}
	// The hub is internal in tests; a restricted project needs a hub that
	// qualifies, so give the roster nothing and rely on the access check
	// firing first.
	guest := Request{ConversationID: "chat", Input: "hello", SenderOpenID: "ou_guest", MessageID: "m1"}
	_, err := coordinator.Handle(ctx, guest)
	if err == nil || !strings.Contains(err.Error(), "none") {
		t.Fatalf("guest on a restricted project = %v; want a role refusal", err)
	}
	// A guest cannot grant; the owner can.
	if result, _ := coordinator.Handle(ctx, Request{ConversationID: "chat", Input: "/grant codex ou_guest write", SenderOpenID: "ou_guest"}); result.Text != "" && !strings.Contains(result.Text, "none") {
		t.Fatalf("guest grant = %q", result.Text)
	}
	result, err := coordinator.Handle(ctx, Request{ConversationID: "chat", Input: "/grant codex ou_guest write", SenderOpenID: "ou_owner"})
	if err != nil || !strings.Contains(result.Text, "ou_guest") {
		t.Fatalf("owner grant = %#v, %v", result, err)
	}
	result, _ = coordinator.Handle(ctx, Request{ConversationID: "chat", Input: "/grant codex", SenderOpenID: "ou_owner"})
	if !strings.Contains(result.Text, "ou_guest — write") {
		t.Fatalf("grant list = %q", result.Text)
	}
	role, _ := coordinator.projects.Access(ctx, "codex", "ou_guest", "ou_owner")
	if role != project.RoleWrite {
		t.Fatalf("role after grant = %s", role)
	}
	// With write, the same guest's turn is admitted (and then meets the
	// level check, which is a different refusal).
	_, err = coordinator.Handle(ctx, Request{ConversationID: "chat", Input: "hello", SenderOpenID: "ou_guest", MessageID: "m2"})
	if err != nil && strings.Contains(err.Error(), "role") {
		t.Fatalf("granted guest still refused on role: %v", err)
	}
}

// A sealed project's answer waits for the owner; approval sends it to the
// original conversation, denial discards it.
func TestSealedAnswersWaitForTheOwner(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "the secret answer"}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	coordinator.SetIdentity("ou_owner", home.Dir{Path: t.TempDir()})
	ctx := context.Background()
	dir := workspaceOf(t, coordinator, "codex")
	if err := coordinator.projects.Declare(ctx, []project.Project{{ID: "codex", Level: project.LevelSealed, Home: project.Home{Path: dir}, DefaultRole: project.RoleWrite}}); err != nil {
		t.Fatal(err)
	}
	// The test hub must be cleared for sealed data for the turn to run.
	coordinator.fleet = nil
	var sent []TaskNotice
	coordinator.SetNotifier(func(n TaskNotice) { sent = append(sent, n) })

	result, err := coordinator.Handle(ctx, Request{ConversationID: "chat", ChatID: "oc_1", Input: "tell me", SenderOpenID: "ou_guest", MessageID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "secret") || !strings.Contains(result.Text, string(protocol.CommandApprove)) {
		t.Fatalf("sealed answer leaked or no approval hint: %q", result.Text)
	}
	id := result.Text[strings.LastIndex(result.Text, " ")+1:]
	// Not the owner: refused.
	if r, _ := coordinator.Handle(ctx, Request{ConversationID: "chat", Input: "/approve " + id, SenderOpenID: "ou_guest"}); strings.Contains(r.Text, "已批准") {
		t.Fatal("a guest approved a disclosure")
	}
	if len(sent) != 0 {
		t.Fatal("content left before approval")
	}
	r, err := coordinator.Handle(ctx, Request{ConversationID: "dm", Input: "/approve " + id, SenderOpenID: "ou_owner"})
	if err != nil || !strings.Contains(r.Text, id) {
		t.Fatalf("owner approve = %#v %v", r, err)
	}
	if len(sent) != 1 || sent[0].Text != "the secret answer" || sent[0].MessageID != "m1" || sent[0].ChatID != "oc_1" {
		t.Fatalf("released = %+v", sent)
	}
	// Once resolved, the id is gone.
	if r, _ := coordinator.Handle(ctx, Request{ConversationID: "dm", Input: "/deny " + id, SenderOpenID: "ou_owner"}); !strings.Contains(r.Text, id) || strings.Contains(r.Text, "已拒绝") {
		t.Fatalf("second decision = %q", r.Text)
	}
}
