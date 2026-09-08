package turn

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
)

func TestFirstConversationContinuesAfterIdentityGeneration(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "owner"); err != nil {
		t.Fatal(err)
	}
	c, store, runner := homeCoordinator(t, dir, "owner")
	runner.reply = "你好，资料已记下。\n===SOUL.md===\n# Soul\n你是帮助用户维护项目的可靠助手。\n===USER.md===\n# User\n- 称呼：李工\n- 时区：Asia/Shanghai\n"
	req := Request{ConversationID: "dm", Input: "你好", SenderOpenID: "owner", ChatType: protocol.ChatP2P}
	first, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if home.NeedsInit(dir) {
		t.Fatal("first message did not generate identity")
	}
	upstream := first.Injected.Session
	runner.reply = "我可以协助维护项目。"
	req.Input = "你能做什么"
	second, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatalf("natural followup after onboarding: %v", err)
	}
	if second.Injected.NewSession || second.Injected.Session != upstream {
		t.Fatal("identity refresh discarded conversation history")
	}
	if !second.Injected.InstructionsSent || !strings.Contains(second.Injected.Instructions, "李工") {
		t.Fatal("updated identity was not sent")
	}
	if second.Injected.Prompt != "[steve: speaker=owner owner=true chat=p2p]\n你能做什么" {
		t.Fatalf("followup was replaced: %q", second.Injected.Prompt)
	}
	third, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if third.Injected.InstructionsSent {
		t.Fatal("unchanged identity injected again")
	}
	if len(runner.seen()) != 3 || len(store.Conversation("dm").Archived) != 0 {
		t.Fatal("refresh replayed or archived the conversation")
	}
}

func TestIdentityEditRefreshesOnlySettledSession(t *testing.T) {
	for _, tainted := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "uncertain"}[tainted], func(t *testing.T) {
			dir := t.TempDir()
			if err := home.Bootstrap(dir, "owner"); err != nil {
				t.Fatal(err)
			}
			if err := home.WriteIdentity(dir, "# Soul\noriginal assistant", "# User\noriginal name"); err != nil {
				t.Fatal(err)
			}
			c, store, runner := homeCoordinator(t, dir, "owner")
			req := Request{ConversationID: "dm", Input: "hello", SenderOpenID: "owner", ChatType: protocol.ChatP2P}
			first, err := c.Handle(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			session := store.Conversation("dm").Sessions["codex"]
			session.Tainted = tainted
			if err := store.SaveSession(session); err != nil {
				t.Fatal(err)
			}
			if err := home.WriteIdentity(dir, "# Soul\nupdated assistant", "# User\nupdated name"); err != nil {
				t.Fatal(err)
			}
			second, err := c.Handle(t.Context(), req)
			if tainted {
				if err == nil || len(runner.seen()) != 1 {
					t.Fatal("uncertain execution was retried")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if second.Injected.Session != first.Injected.Session || !strings.Contains(second.Injected.Instructions, "updated name") {
				t.Fatal("profile edit lost history or stale identity")
			}
		})
	}
}

func TestIdentityRefreshAfterReopeningSessionState(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted-baseline", true: "unknown-baseline"}[missing], func(t *testing.T) {
			dir := t.TempDir()
			if err := home.Bootstrap(dir, "owner"); err != nil {
				t.Fatal(err)
			}
			c, _, manager := homeCoordinatorWithManager(t, dir, "owner")
			path := filepath.Join(t.TempDir(), "state.json")
			persisted, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			c.store = persisted
			req := Request{ConversationID: "dm", Input: "hello", SenderOpenID: "owner", ChatType: protocol.ChatP2P}
			first, err := c.Handle(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if missing {
				saved := persisted.Conversation("dm").Sessions["codex"]
				saved.SessionConfigHash = ""
				if err := persisted.SaveSession(saved); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			next := restartCoordinator(t, c, c.catalog, reopened, c.assembler, manager, time.Minute)
			next.SetIdentity("owner", home.Dir{Path: dir})
			if err := home.WriteIdentity(dir, "updated soul", "updated name"); err != nil {
				t.Fatal(err)
			}
			second, err := next.Handle(t.Context(), req)
			if missing {
				if err == nil || len(manager.runners["codex"].seen()) != 1 {
					t.Fatal("unclassified drift dispatched a new prompt")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if second.Injected.Session != first.Injected.Session || second.Injected.NewSession || !second.Injected.InstructionsSent || !strings.Contains(second.Injected.Instructions, "updated name") {
				t.Fatal("restart lost the identity refresh baseline or conversation")
			}
		})
	}
}
