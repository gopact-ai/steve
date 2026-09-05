package agentmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type fakeSender struct {
	mu      sync.Mutex
	cards   []string // "anchorID:payload"
	texts   []string // "anchorID:text"
	patches []string // "messageID:payload"
	deleted []string
	fail    bool
	timeout bool
	next    int
}

func (f *fakeSender) id() string {
	f.next++
	return fmt.Sprintf("om_sent_%d", f.next)
}

func (f *fakeSender) ReplyCard(_ context.Context, messageID string, payload []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timeout {
		return "", context.DeadlineExceeded
	}
	if f.fail {
		return "", fmt.Errorf("boom")
	}
	f.cards = append(f.cards, messageID+":"+string(payload))
	return f.id(), nil
}

func (f *fakeSender) ReplyText(_ context.Context, messageID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", fmt.Errorf("boom")
	}
	f.texts = append(f.texts, messageID+":"+text)
	return f.id(), nil
}

func (f *fakeSender) PatchCard(_ context.Context, messageID string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return fmt.Errorf("boom")
	}
	f.patches = append(f.patches, messageID+":"+string(payload))
	return nil
}

func (f *fakeSender) DeleteMessage(_ context.Context, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, messageID)
	return nil
}

func startServer(t *testing.T) (*Server, *fakeSender) {
	t.Helper()
	s, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	s.BindChannel(sender)
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

type rpcResult struct {
	status  int
	body    map[string]any
	rawBody string
}

func rpc(t *testing.T, url, token, method string, params any) rpcResult {
	t.Helper()
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		payload["params"] = params
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := rpcResult{status: resp.StatusCode, rawBody: buf.String()}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(buf.Bytes(), &out.body); err != nil {
			t.Fatalf("bad rpc response %q: %v", buf.String(), err)
		}
	}
	return out
}

func callTool(t *testing.T, url, token, name string, args map[string]any) (string, bool) {
	t.Helper()
	out := rpc(t, url, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if out.status != http.StatusOK {
		t.Fatalf("tools/call status %d: %s", out.status, out.rawBody)
	}
	result, ok := out.body["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", out.body)
	}
	isError, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	text := ""
	if len(content) > 0 {
		if first, ok := content[0].(map[string]any); ok {
			text, _ = first["text"].(string)
		}
	}
	return text, isError
}

func register(s *Server, conversation, agent, token, anchorMessage string) {
	s.Extras(conversation, agent, token, "")
	s.Anchor(conversation, conversation, anchorMessage)
}

func TestRejectsMissingOrUnknownToken(t *testing.T) {
	s, _ := startServer(t)
	for _, token := range []string{"", "tok-unknown"} {
		out := rpc(t, s.URL(), token, "tools/list", nil)
		if out.status != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d, want 401", token, out.status)
		}
	}
}

func TestInitializeAndToolsList(t *testing.T) {
	s, _ := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_1")
	out := rpc(t, s.URL(), "tok-a", "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "codex", "version": "0"},
	})
	result := out.body["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocol not echoed: %v", result)
	}
	list := rpc(t, s.URL(), "tok-a", "tools/list", nil)
	if !strings.Contains(list.rawBody, "feishu_send") || !strings.Contains(list.rawBody, "feishu_recall") {
		t.Fatalf("tools missing: %s", list.rawBody)
	}
}

func TestNotificationAccepted(t *testing.T) {
	s, _ := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_1")
	req, _ := http.NewRequest(http.MethodPost, s.URL(),
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer tok-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status %d, want 202", resp.StatusCode)
	}
}

func TestSendDeliversToOwnConversationAnchor(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	register(s, "oc_b", "codex", "tok-b", "om_b")
	text, isError := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "## milestone\ndone phase 1"})
	if isError || !strings.Contains(text, "sent message_id=om_sent_1") {
		t.Fatalf("send failed: %q isError=%v", text, isError)
	}
	if len(sender.cards) != 1 || !strings.HasPrefix(sender.cards[0], "om_a:") {
		t.Fatalf("card went to the wrong anchor: %v", sender.cards)
	}
	if !strings.Contains(sender.cards[0], "milestone") || !strings.Contains(sender.cards[0], `"schema":"2.0"`) {
		t.Fatalf("card payload wrong: %v", sender.cards[0])
	}
}

func TestSendTextFormat(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	text, isError := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "plain note", "format": "text"})
	if isError || !strings.Contains(text, "sent message_id=") {
		t.Fatalf("text send failed: %q", text)
	}
	if len(sender.texts) != 1 || sender.texts[0] != "om_a:plain note" {
		t.Fatalf("text not sent: %v", sender.texts)
	}
}

func TestSendRejectsMention(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	text, isError := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "hello", "mention": true})
	if !isError || !strings.Contains(text, "mention") {
		t.Fatalf("mention was not rejected: %q", text)
	}
	if len(sender.cards)+len(sender.texts) != 0 {
		t.Fatal("rejected send still delivered")
	}
}

func TestSendStripsAtMarkup(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	_, isError := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{
		"content": "ping <at id=all>everyone</at> and <at user_id=\"ou_x\">bob</at> now",
	})
	if isError {
		t.Fatal("send failed")
	}
	if len(sender.cards) != 1 || strings.Contains(strings.ToLower(sender.cards[0]), "<at") {
		t.Fatalf("at markup survived: %v", sender.cards)
	}
	if !strings.Contains(sender.cards[0], "everyone") {
		t.Fatalf("inner text lost: %v", sender.cards)
	}
}

func TestSendWithoutAnchorFails(t *testing.T) {
	s, sender := startServer(t)
	s.Extras("oc_a", "codex", "tok-a", "")
	text, isError := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "hello"})
	if !isError || !strings.Contains(text, "no active conversation") {
		t.Fatalf("anchorless send not refused: %q", text)
	}
	if len(sender.cards) != 0 {
		t.Fatal("anchorless send delivered")
	}
}

func TestSendLimitPerTurn(t *testing.T) {
	s, _ := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	for i := 0; i < maxSendsPerTurn; i++ {
		if _, isError := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "m"}); isError {
			t.Fatalf("send %d refused", i)
		}
	}
	text, isError := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "m"})
	if !isError || !strings.Contains(text, "limit") {
		t.Fatalf("limit not enforced: %q", text)
	}
	// A new turn resets the budget.
	s.Anchor("oc_a", "oc_a", "om_a2")
	if _, isError := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "m"}); isError {
		t.Fatal("new turn did not reset the send budget")
	}
}

func TestRecallOwnMessageOnly(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	register(s, "oc_b", "codex", "tok-b", "om_b")
	textA, _ := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "from a"})
	idA := strings.TrimPrefix(textA, "sent message_id=")
	// The other conversation's agent cannot recall A's message.
	text, isError := callTool(t, s.URL(), "tok-b", "feishu_recall", map[string]any{"message_id": idA})
	if !isError || !strings.Contains(text, "current turn") {
		t.Fatalf("cross-conversation recall allowed: %q", text)
	}
	if len(sender.deleted) != 0 {
		t.Fatal("cross-conversation recall deleted a message")
	}
	// The owner can.
	text, isError = callTool(t, s.URL(), "tok-a", "feishu_recall", map[string]any{"message_id": idA})
	if isError || !strings.Contains(text, "recalled") {
		t.Fatalf("own recall failed: %q", text)
	}
	if len(sender.deleted) != 1 || sender.deleted[0] != idA {
		t.Fatalf("wrong message deleted: %v", sender.deleted)
	}
	// And only once.
	if _, isError = callTool(t, s.URL(), "tok-a", "feishu_recall", map[string]any{"message_id": idA}); !isError {
		t.Fatal("double recall allowed")
	}
}

func TestRecallStopsAtTurnBoundary(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	text, _ := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "from a"})
	id := strings.TrimPrefix(text, "sent message_id=")
	s.Anchor("oc_a", "oc_a", "om_a2") // next turn begins
	out, isError := callTool(t, s.URL(), "tok-a", "feishu_recall", map[string]any{"message_id": id})
	if !isError || !strings.Contains(out, "current turn") {
		t.Fatalf("stale recall allowed: %q", out)
	}
	if len(sender.deleted) != 0 {
		t.Fatal("stale recall deleted a message")
	}
}

func TestTokenRotationRevokesOldToken(t *testing.T) {
	s, _ := startServer(t)
	register(s, "oc_a", "codex", "tok-old", "om_a")
	s.Extras("oc_a", "codex", "tok-new", "")
	if out := rpc(t, s.URL(), "tok-old", "tools/list", nil); out.status != http.StatusUnauthorized {
		t.Fatalf("old token still works: %d", out.status)
	}
	if out := rpc(t, s.URL(), "tok-new", "tools/list", nil); out.status != http.StatusOK {
		t.Fatalf("new token refused: %d", out.status)
	}
}

func TestGetRefused(t *testing.T) {
	s, _ := startServer(t)
	resp, err := http.Get(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d, want 405", resp.StatusCode)
	}
}

func TestPreferredPortReusedAcrossRestarts(t *testing.T) {
	first, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	port := first.Port()
	if port <= 0 {
		t.Fatalf("no port reported: %d", port)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = first.Start(ctx) }()
	cancel()
	<-done
	second, err := New(port)
	if err != nil {
		t.Fatal(err)
	}
	if second.Port() != port {
		t.Fatalf("restart lost the port: %d -> %d", port, second.Port())
	}
	// A port that is meanwhile taken falls back instead of failing.
	third, err := New(port)
	if err != nil {
		t.Fatal(err)
	}
	if third.Port() == port {
		t.Fatalf("two servers on one port")
	}
}

func TestUpdateRewritesOwnMilestoneCard(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	text, _ := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "v1"})
	id := strings.TrimPrefix(text, "sent message_id=")
	out, isError := callTool(t, s.URL(), "tok-a", "feishu_update", map[string]any{"message_id": id, "content": "## v2 进度"})
	if isError || !strings.Contains(out, "updated "+id) {
		t.Fatalf("update failed: %q", out)
	}
	if len(sender.patches) != 1 || !strings.HasPrefix(sender.patches[0], id+":") || !strings.Contains(sender.patches[0], "v2 进度") {
		t.Fatalf("patch wrong: %v", sender.patches)
	}
	// An updated card can still be recalled.
	if out, isError = callTool(t, s.URL(), "tok-a", "feishu_recall", map[string]any{"message_id": id}); isError {
		t.Fatalf("recall after update failed: %q", out)
	}
}

func TestUpdateRejectsTextAndForeignAndStale(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	register(s, "oc_b", "codex", "tok-b", "om_b")
	// Text messages cannot become cards.
	text, _ := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "plain", "format": "text"})
	textID := strings.TrimPrefix(text, "sent message_id=")
	if out, isError := callTool(t, s.URL(), "tok-a", "feishu_update", map[string]any{"message_id": textID, "content": "x"}); !isError || !strings.Contains(out, "markdown") {
		t.Fatalf("text update not refused: %q", out)
	}
	// Another conversation's agent cannot update it either.
	card, _ := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "card"})
	cardID := strings.TrimPrefix(card, "sent message_id=")
	if out, isError := callTool(t, s.URL(), "tok-b", "feishu_update", map[string]any{"message_id": cardID, "content": "x"}); !isError || !strings.Contains(out, "current turn") {
		t.Fatalf("foreign update not refused: %q", out)
	}
	// A new turn ends updatability.
	s.Anchor("oc_a", "oc_a", "om_a2")
	if out, isError := callTool(t, s.URL(), "tok-a", "feishu_update", map[string]any{"message_id": cardID, "content": "x"}); !isError || !strings.Contains(out, "current turn") {
		t.Fatalf("stale update not refused: %q", out)
	}
	if len(sender.patches) != 0 {
		t.Fatalf("refused updates still patched: %v", sender.patches)
	}
}

func TestMilestoneTailStyleAndProgress(t *testing.T) {
	s, sender := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	// Before the platform reports its identity line, the agent id is the tail.
	callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "m1"})
	if !strings.Contains(sender.cards[0], "codex · 里程碑 1") {
		t.Fatalf("auto-numbered tail missing: %v", sender.cards[0])
	}
	s.SetStyle("oc_a", "codex · GPT X · Agent")
	text, _ := callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "m2", "progress": "2/3"})
	id := strings.TrimPrefix(text, "sent message_id=")
	if !strings.Contains(sender.cards[1], "codex · GPT X · Agent · 里程碑 2/3") {
		t.Fatalf("styled progress tail missing: %v", sender.cards[1])
	}
	// An update without progress keeps the card's own badge; with progress
	// it advances.
	callTool(t, s.URL(), "tok-a", "feishu_update", map[string]any{"message_id": id, "content": "m2b"})
	if !strings.Contains(sender.patches[0], "里程碑 2/3") {
		t.Fatalf("update lost the badge: %v", sender.patches[0])
	}
	callTool(t, s.URL(), "tok-a", "feishu_update", map[string]any{"message_id": id, "content": "m2c", "progress": "3/3"})
	if !strings.Contains(sender.patches[1], "里程碑 3/3") {
		t.Fatalf("update did not advance the badge: %v", sender.patches[1])
	}
}

func TestInterimTracksCurrentEpoch(t *testing.T) {
	s, _ := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	if s.Interim("oc_a") {
		t.Fatal("interim before any send")
	}
	callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "m"})
	if !s.Interim("oc_a") {
		t.Fatal("interim not reported after a send")
	}
	if s.Interim("oc_other") {
		t.Fatal("interim leaked across conversations")
	}
	s.Anchor("oc_a", "oc_a", "om_a2")
	if s.Interim("oc_a") {
		t.Fatal("interim survived the turn boundary")
	}
}

func TestJournalSeesEverySend(t *testing.T) {
	s, _ := startServer(t)
	register(s, "oc_a", "codex", "tok-a", "om_a")
	var mu sync.Mutex
	var seen []string
	s.SetJournal(func(conversationID, agentID, messageID string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, conversationID+":"+agentID+":"+messageID)
	})
	callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "card"})
	callTool(t, s.URL(), "tok-a", "feishu_send", map[string]any{"content": "plain", "format": "text"})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "oc_a:codex:om_sent_1" || seen[1] != "oc_a:codex:om_sent_2" {
		t.Fatalf("journal = %v", seen)
	}
}
