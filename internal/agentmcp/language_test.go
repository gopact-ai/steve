package agentmcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/memory"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

func startServerIn(t *testing.T, text i18n.Catalog) (*Server, *fakeSender) {
	t.Helper()
	s, err := New(0, text)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	s.BindChannel("feishu", feishu.Messenger{API: sender})
	s.SetDefaultChannel("feishu")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Start(ctx); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s, sender
}

// A milestone card's footer — its stage and who delegated it — is worn
// in the server's language.
func TestMilestoneFooterIsInTheServerLanguage(t *testing.T) {
	s, sender := startServerIn(t, i18n.New(i18n.LocaleEN))
	register(s, "oc_a", "codex", "tok-a", "om_a")
	callTool(t, s.URL(), "tok-a", "channel_send", map[string]any{"content": "m1"})
	callTool(t, s.URL(), "tok-a", "channel_send", map[string]any{"content": "m2", "progress": "2/3"})
	s.Delegated("oc_a", "child", "t1", "parent", "tok-c", "")
	callTool(t, s.URL(), "tok-c", "channel_send", map[string]any{"content": "m3"})
	if len(sender.cards) != 3 {
		t.Fatalf("cards = %v", sender.cards)
	}
	for i, want := range []string{"codex · Milestone 1", "codex · Milestone 2/3", "child · delegated by parent · Milestone 1"} {
		if !strings.Contains(sender.cards[i], want) || containsHan(sender.cards[i]) {
			t.Fatalf("card %d = %s, want footer %q with no Chinese", i, sender.cards[i], want)
		}
	}
}

// What steve_remember and steve_node_remove report back is in the
// server's language.
func TestToolRepliesAreInTheServerLanguage(t *testing.T) {
	dir := t.TempDir()
	svc := memory.NewService(memory.NewMarkdown("", dir), filepath.Join(dir, "audit.jsonl"))
	s, _ := startServerIn(t, i18n.New(i18n.LocaleEN))
	s.SetMemorizer(testMemorizer{svc: svc})
	bind := binding{conversationID: "chat", agentID: "codex"}
	for _, text := range []string{"first fact", "first fact"} {
		out, err := s.steveRemember(t.Context(), bind, json.RawMessage(`{"scope":"project","text":"`+text+`"}`))
		if err != nil || containsHan(out) || !strings.Contains(out, `"note"`) {
			t.Fatalf("steve_remember = %s, %v; want an English note", out, err)
		}
	}
	s.SetFleeter(removingFleeter{})
	s.SetInformer(&taskInformer{})
	s.Extras("chat", "agent", "token", "")
	out, bad := callTool(t, s.URL(), "token", "steve_node_remove", map[string]any{"name": "worker"})
	if bad || containsHan(out) || !strings.Contains(out, "worker") {
		t.Fatalf("steve_node_remove = %q (error %v), want an English reply naming the node", out, bad)
	}
}

// A full scope warns in the server's language.
func TestRememberWarningIsInTheServerLanguage(t *testing.T) {
	s, err := New(0, i18n.New(i18n.LocaleEN))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.listener.Close() })
	s.SetMemorizer(fullMemorizer{})
	out, err := s.steveRemember(t.Context(), binding{conversationID: "chat", agentID: "codex"}, json.RawMessage(`{"scope":"project","text":"x"}`))
	if err != nil || !strings.Contains(out, `"warning"`) || containsHan(out) {
		t.Fatalf("steve_remember = %s, %v; want an English warning", out, err)
	}
}

// Every tool the platform offers has a label in every language.
func TestToolTitlesAreCatalogEntries(t *testing.T) {
	for name, key := range ToolTitles() {
		zh, en := i18n.New(i18n.LocaleZH).T(key), i18n.New(i18n.LocaleEN).T(key)
		if zh == string(key) || en == string(key) || containsHan(en) {
			t.Fatalf("%s: zh %q, en %q", name, zh, en)
		}
	}
}

type removingFleeter struct{ Fleeter }

func (removingFleeter) RemoveNode(context.Context, string) error { return nil }

type fullMemorizer struct{ Memorizer }

func (fullMemorizer) Remember(context.Context, string, string, string, string, string, string, string) (memory.Receipt, memory.Scope, error) {
	return memory.Receipt{ID: "m1", New: true, Bytes: 95, Budget: 100}, memory.ProjectScope("test"), nil
}
