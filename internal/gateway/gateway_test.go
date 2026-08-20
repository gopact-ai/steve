package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
)

type recordingChannel struct {
	events chan string
	addErr error
}

func (c *recordingChannel) AddReaction(_ context.Context, messageID, emoji string) (string, error) {
	if c.addErr != nil {
		c.events <- "add-err"
		return "", c.addErr
	}
	c.events <- "add:" + messageID + ":" + emoji
	return "rx_1", nil
}

func (c *recordingChannel) RemoveReaction(_ context.Context, messageID, reactionID string) error {
	c.events <- "remove:" + messageID + ":" + reactionID
	return nil
}

func (c *recordingChannel) Reply(_ context.Context, messageID, text string) error {
	c.events <- "reply:" + messageID + ":" + text
	return nil
}

func collectEvents(t *testing.T, events <-chan string, n int) []string {
	t.Helper()
	got := make([]string, 0, n)
	deadline := time.After(time.Second)
	for len(got) < n {
		select {
		case ev := <-events:
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for %d events, got %v", n, got)
		}
	}
	return got
}

type reply struct{ text chan string }

func (r *reply) Reply(_ context.Context, _, text string) error {
	r.text <- text
	return nil
}

type fakeProcessor struct{}

func (fakeProcessor) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	return turn.Result{Text: "reply: " + req.Input}, nil
}

type cancelingProcessor struct{}

func (cancelingProcessor) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, context.Canceled
}

type countingProcessor struct{ calls atomic.Int32 }

func (p *countingProcessor) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	p.calls.Add(1)
	return turn.Result{Text: "reply: " + req.Input}, nil
}

type userErrorProcessor struct{}

func (userErrorProcessor) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, turn.UserError{Text: i18n.New(i18n.LocaleZH).T(i18n.CapabilityDrift, protocol.CommandNew)}
}

func TestGatewayReplies(t *testing.T) {
	g := New(fakeProcessor{})
	r := &reply{text: make(chan string, 1)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"})

	select {
	case got := <-r.text:
		if got != "reply: hello" {
			t.Fatalf("unexpected reply: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reply")
	}
}

func TestGatewayRepliesUserError(t *testing.T) {
	g := New(userErrorProcessor{})
	r := &reply{text: make(chan string, 1)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"})

	select {
	case got := <-r.text:
		if got != i18n.New(i18n.LocaleZH).T(i18n.CapabilityDrift, protocol.CommandNew) {
			t.Fatalf("unexpected reply: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reply")
	}
}

func TestGatewayRepliesCanceledTurn(t *testing.T) {
	g := New(cancelingProcessor{})
	r := &reply{text: make(chan string, 1)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "long task"})

	select {
	case got := <-r.text:
		if got != i18n.New(i18n.LocaleZH).T(i18n.TurnCanceled) {
			t.Fatalf("unexpected reply: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reply")
	}
}

func TestGatewayDeduplicatesMessageID(t *testing.T) {
	processor := &countingProcessor{}
	g := New(processor)
	r := &reply{text: make(chan string, 2)}
	g.BindChannel(r)
	msg := feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"}
	g.HandleMessage(msg)
	g.HandleMessage(msg)
	select {
	case <-r.text:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reply")
	}
	time.Sleep(20 * time.Millisecond)
	if calls := processor.calls.Load(); calls != 1 {
		t.Fatalf("processor calls = %d, want 1", calls)
	}
}

type blockingProcessor struct {
	mu      sync.Mutex
	seen    []string
	release chan struct{}
}

func (p *blockingProcessor) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	first := false
	p.mu.Lock()
	p.seen = append(p.seen, req.Input)
	first = len(p.seen) == 1
	p.mu.Unlock()
	if first {
		<-p.release
	}
	return turn.Result{Text: "reply: " + req.Input}, nil
}

func (p *blockingProcessor) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func TestGatewayCancelDrainsQueuedMessages(t *testing.T) {
	processor := &blockingProcessor{release: make(chan struct{})}
	g := New(processor)
	r := &reply{text: make(chan string, 4)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_1", Text: "long task"})
	deadline := time.Now().Add(time.Second)
	for len(processor.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(processor.texts()) == 0 {
		t.Fatal("worker did not pick up the first message")
	}
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_2", Text: "queued"})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_3", Text: "/cancel"})
	close(processor.release)

	deadline = time.Now().Add(time.Second)
	for len(processor.texts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for _, seen := range processor.texts() {
		if seen == "queued" {
			t.Fatal("queued message ran after cancel")
		}
	}
	if got := processor.texts(); len(got) < 2 || got[0] != "long task" || got[1] != "/cancel" {
		t.Fatalf("unexpected processing order: %v", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	g := New(fakeProcessor{})
	if got := g.truncateRunes("short", maxReplyRunes); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
	long := strings.Repeat("长", maxReplyRunes+1)
	got := g.truncateRunes(long, maxReplyRunes)
	if !strings.HasPrefix(got, strings.Repeat("长", maxReplyRunes)) {
		t.Fatal("truncated text lost its prefix")
	}
	if !strings.Contains(got, i18n.New(i18n.LocaleZH).T(i18n.Truncated)) {
		t.Fatal("truncated text is missing the truncation notice")
	}
}

func TestGatewayAcksThenClearsReaction(t *testing.T) {
	processor := &blockingProcessor{release: make(chan struct{})}
	g := New(processor)
	events := make(chan string, 8)
	g.BindChannel(&recordingChannel{events: events})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"})

	deadline := time.Now().Add(time.Second)
	for len(processor.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(processor.texts()) == 0 {
		t.Fatal("processor did not start")
	}
	select {
	case ev := <-events:
		if ev != "add:om_message:THINKING" {
			t.Fatalf("ack = %q", ev)
		}
	default:
		t.Fatal("missing ack reaction before the turn finished")
	}
	select {
	case ev := <-events:
		t.Fatalf("unexpected event while thinking: %s", ev)
	default:
	}

	close(processor.release)
	got := collectEvents(t, events, 2)
	if got[0] != "reply:om_message:reply: hello" {
		t.Fatalf("reply = %q", got[0])
	}
	if got[1] != "remove:om_message:rx_1" {
		t.Fatalf("clear = %q", got[1])
	}
}

func TestGatewayReplyWithoutReactionWhenAckFails(t *testing.T) {
	g := New(fakeProcessor{})
	events := make(chan string, 8)
	g.BindChannel(&recordingChannel{events: events, addErr: errors.New("denied")})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"})

	got := collectEvents(t, events, 2)
	if got[0] != "add-err" || got[1] != "reply:om_message:reply: hello" {
		t.Fatalf("events = %v", got)
	}
	select {
	case ev := <-events:
		t.Fatalf("unexpected extra event: %s", ev)
	case <-time.After(20 * time.Millisecond):
	}
}

type emptyProcessor struct{}

func (emptyProcessor) Handle(context.Context, turn.Request) (turn.Result, error) {
	return turn.Result{}, nil
}

func TestGatewaySilentListenSkipsEmptyUnmentionedGroup(t *testing.T) {
	g := New(emptyProcessor{})
	events := make(chan string, 8)
	g.BindChannel(&recordingChannel{events: events})
	g.HandleMessage(feishu.InboundMessage{
		ChatID: "oc_chat", ChatType: protocol.ChatGroup, MessageID: "om_message", Text: "side chat",
	})
	select {
	case ev := <-events:
		t.Fatalf("unmentioned empty listen replied: %s", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

type recordingCards struct {
	events   chan string
	startErr error
}

func (c *recordingCards) AddReaction(_ context.Context, messageID, emoji string) (string, error) {
	c.events <- "add:" + messageID + ":" + emoji
	return "rx_1", nil
}

func (c *recordingCards) RemoveReaction(_ context.Context, messageID, reactionID string) error {
	c.events <- "remove:" + messageID + ":" + reactionID
	return nil
}

func (c *recordingCards) Reply(_ context.Context, messageID, text string) error {
	c.events <- "reply:" + messageID + ":" + text
	return nil
}

func (c *recordingCards) ReplyCard(_ context.Context, messageID string, payload []byte) (string, error) {
	if c.startErr != nil {
		c.events <- "card-err"
		return "", c.startErr
	}
	c.events <- "card:" + messageID + ":" + cardStatus(payload)
	return "om_card", nil
}

func (c *recordingCards) PatchCard(_ context.Context, messageID string, payload []byte) error {
	c.events <- "patch:" + messageID + ":" + cardStatus(payload)
	return nil
}

func (c *recordingCards) DeleteMessage(_ context.Context, messageID string) error {
	c.events <- "delete:" + messageID
	return nil
}

func cardStatus(payload []byte) string {
	body := string(payload)
	switch {
	case strings.Contains(body, "已取消"):
		return "cancelled"
	case strings.Contains(body, "失败"):
		return "failed"
	case strings.Contains(body, "完成"):
		return "completed"
	default:
		return "running"
	}
}

func TestGatewaySendsOneCard(t *testing.T) {
	g := New(fakeProcessor{})
	events := make(chan string, 8)
	g.BindChannel(&recordingCards{events: events})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"})

	got := collectEvents(t, events, 4)
	if got[0] != "add:om_message:THINKING" {
		t.Fatalf("ack = %q", got[0])
	}
	if got[1] != "card:om_message:running" {
		t.Fatalf("start = %q", got[1])
	}
	if got[2] != "patch:om_card:completed" {
		t.Fatalf("finish = %q", got[2])
	}
	if got[3] != "remove:om_message:rx_1" {
		t.Fatalf("unack = %q", got[3])
	}
	select {
	case ev := <-events:
		t.Fatalf("extra event: %s", ev)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestGatewayCardStartFailsFallsBackToText(t *testing.T) {
	g := New(fakeProcessor{})
	events := make(chan string, 8)
	g.BindChannel(&recordingCards{events: events, startErr: errors.New("denied")})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"})

	got := collectEvents(t, events, 4)
	if got[0] != "add:om_message:THINKING" || got[1] != "card-err" || got[2] != "reply:om_message:reply: hello" || got[3] != "remove:om_message:rx_1" {
		t.Fatalf("events = %v", got)
	}
}

func TestGatewayCardSilentListenSkipsEmpty(t *testing.T) {
	g := New(emptyProcessor{})
	events := make(chan string, 8)
	g.BindChannel(&recordingCards{events: events})
	g.HandleMessage(feishu.InboundMessage{
		ChatID: "oc_chat", ChatType: protocol.ChatGroup, MessageID: "om_message", Text: "side chat",
	})
	select {
	case ev := <-events:
		t.Fatalf("unmentioned empty listen used a card: %s", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestGatewayClearsReactionAfterFailedTurn(t *testing.T) {
	g := New(cancelingProcessor{})
	events := make(chan string, 8)
	g.BindChannel(&recordingChannel{events: events})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "long task"})

	got := collectEvents(t, events, 3)
	want := []string{
		"add:om_message:THINKING",
		"reply:om_message:" + i18n.New(i18n.LocaleZH).T(i18n.TurnCanceled),
		"remove:om_message:rx_1",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

type captureProcessor struct{ req chan turn.Request }

func (p *captureProcessor) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	p.req <- req
	return turn.Result{Text: "ok"}, nil
}

func TestGatewayForwardsImages(t *testing.T) {
	p := &captureProcessor{req: make(chan turn.Request, 1)}
	g := New(p)
	g.BindChannel(&reply{text: make(chan string, 1)})
	g.HandleMessage(feishu.InboundMessage{
		ChatID: "oc_chat", MessageID: "om_message",
		Images: []feishu.Image{{MIME: "image/png", Data: []byte("png")}},
	})
	req := <-p.req
	if req.Input != i18n.New(i18n.LocaleZH).T(i18n.ImagePlaceholder) {
		t.Fatalf("input = %q", req.Input)
	}
	if len(req.Images) != 1 || string(req.Images[0].Data) != "png" {
		t.Fatalf("images = %#v", req.Images)
	}
}

func TestGatewayIncludesQuotedMessage(t *testing.T) {
	p := &captureProcessor{req: make(chan turn.Request, 1)}
	g := New(p)
	g.BindChannel(&reply{text: make(chan string, 1)})
	g.HandleMessage(feishu.InboundMessage{
		ChatID: "oc_chat", MessageID: "om_message",
		Text: "再试下这个", Quote: "我不喜欢卡片带 title",
	})
	req := <-p.req
	if !strings.Contains(req.Input, "我不喜欢卡片带 title") || !strings.Contains(req.Input, "再试下这个") {
		t.Fatalf("input = %q", req.Input)
	}
}

func TestGatewayRetryActionRerunsMessage(t *testing.T) {
	p := &captureProcessor{req: make(chan turn.Request, 2)}
	g := New(p)
	g.BindChannel(&reply{text: make(chan string, 2)})
	id := g.registerTurn(feishu.InboundMessage{
		ChatID: "oc_chat", MessageID: "om_message", SenderOpenID: "ou_sender", Text: "hello",
	})

	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "turn_retry", RequestID: id,
	}); toast.Type != "info" {
		t.Fatalf("running turn should not be retryable: %#v", toast)
	}

	g.setTurnCard(id, "om_card")
	g.finishTurn(id, true)
	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "turn_retry", RequestID: id,
	}); toast.Type != "success" {
		t.Fatalf("retry toast = %#v", toast)
	}
	if req := <-p.req; req.Input != "hello" {
		t.Fatalf("retry input = %q", req.Input)
	}
	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "turn_retry", RequestID: id,
	}); toast.Type != "info" {
		t.Fatalf("retry should be single use: %#v", toast)
	}
}

func TestGatewayRetryRecallsOldCardFirst(t *testing.T) {
	g := New(fakeProcessor{})
	events := make(chan string, 8)
	g.BindChannel(&recordingCards{events: events})
	id := g.registerTurn(feishu.InboundMessage{
		ChatID: "oc_chat", MessageID: "om_message", SenderOpenID: "ou_sender", Text: "hello",
	})
	g.setTurnCard(id, "om_old_card")
	g.finishTurn(id, true)

	g.HandleCardAction(feishu.CardAction{OpenID: "ou_sender", Action: "turn_retry", RequestID: id})

	got := collectEvents(t, events, 3)
	if got[0] != "delete:om_old_card" {
		t.Fatalf("old card was not recalled first: %v", got)
	}
	if got[1] != "add:om_message:THINKING" || got[2] != "card:om_message:running" {
		t.Fatalf("retry did not send a new card: %v", got)
	}
}

func TestGatewayStopActionCancelsTurn(t *testing.T) {
	p := &captureProcessor{req: make(chan turn.Request, 2)}
	g := New(p)
	g.BindChannel(&reply{text: make(chan string, 2)})
	id := g.registerTurn(feishu.InboundMessage{
		ChatID: "oc_chat", MessageID: "om_message", SenderOpenID: "ou_sender", Text: "long task",
	})

	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_other", Action: "turn_cancel", RequestID: id,
	}); toast.Type != "error" {
		t.Fatalf("another user should not stop the turn: %#v", toast)
	}
	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "turn_cancel", RequestID: id,
	}); toast.Type != "success" {
		t.Fatalf("stop toast = %#v", toast)
	}
	if req := <-p.req; req.Input != string(protocol.CommandCancel) {
		t.Fatalf("stop input = %q", req.Input)
	}
}

func TestGatewayDropsFinishedTurn(t *testing.T) {
	g := New(fakeProcessor{})
	id := g.registerTurn(feishu.InboundMessage{ChatID: "oc_chat", SenderOpenID: "ou_sender"})
	g.finishTurn(id, false)
	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "turn_retry", RequestID: id,
	}); toast.Type != "info" {
		t.Fatalf("succeeded turn should not be retryable: %#v", toast)
	}
}

func TestGatewayHandleCardAction(t *testing.T) {
	g := New(fakeProcessor{})
	done := make(chan string, 1)
	g.asks["req_1"] = &pendingAsk{openID: "ou_sender", cardID: "om_card", done: done}

	toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_other", Action: "tool_approval", RequestID: "req_1", Decision: "allow", MessageID: "om_card",
	})
	if toast.Type != "error" {
		t.Fatalf("wrong user toast = %#v", toast)
	}

	toast = g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "tool_approval", RequestID: "missing", Decision: "allow", MessageID: "om_card",
	})
	if toast.Type != "info" {
		t.Fatalf("expired toast = %#v", toast)
	}

	toast = g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "tool_approval", RequestID: "req_1", Decision: "allow", MessageID: "om_card",
	})
	if toast.Type != "success" {
		t.Fatalf("allow toast = %#v", toast)
	}
	if got := <-done; got != "allow" {
		t.Fatalf("decision = %q", got)
	}
}
