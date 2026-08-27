package gateway

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/card"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
)

// waitDeadline bounds how long a test waits for a goroutine to reach a known
// point. A passing test never spends it; a generous bound is what keeps the
// suite usable under -race, where everything runs several times slower.
const waitDeadline = 10 * time.Second

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
	deadline := time.After(waitDeadline)
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
	case <-time.After(waitDeadline):
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
	case <-time.After(waitDeadline):
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
	case <-time.After(waitDeadline):
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
	case <-time.After(waitDeadline):
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

// Messages no longer wait behind the turn they might be meant to interrupt.
// The gateway hands every one straight to the coordinator, which is where
// taking the turn away from a running prompt is decided.
func TestGatewayStartsMessagesWithoutQueueing(t *testing.T) {
	processor := &blockingProcessor{release: make(chan struct{})}
	g := New(processor)
	r := &reply{text: make(chan string, 4)}
	g.BindChannel(r)
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_1", Text: "long task"})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_2", Text: "actually this"})

	// Both reach the coordinator while the first is still blocked; before,
	// the second sat in a queue until the first finished.
	deadline := time.Now().Add(2 * time.Second)
	for len(processor.texts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(processor.release)
	got := processor.texts()
	if len(got) < 2 {
		t.Fatalf("second message waited for the first: %v", got)
	}
	seen := map[string]bool{}
	for _, text := range got {
		seen[text] = true
	}
	if !seen["long task"] || !seen["actually this"] {
		t.Fatalf("both messages should have started: %v", got)
	}
}

// A redelivered message must still not run twice; dropping the queue did not
// drop the deduplication that sat beside it.
func TestGatewayStillDropsDuplicateMessages(t *testing.T) {
	processor := &blockingProcessor{release: make(chan struct{})}
	close(processor.release)
	g := New(processor)
	g.BindChannel(&reply{text: make(chan string, 4)})
	for i := 0; i < 3; i++ {
		g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_dup", Text: "once"})
	}
	deadline := time.Now().Add(time.Second)
	for len(processor.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := processor.texts(); len(got) != 1 {
		t.Fatalf("duplicate delivered %d times: %v", len(got), got)
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
	sent     int
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
	c.sent++
	id := "om_card"
	if c.sent > 1 {
		id = "om_card2"
	}
	c.events <- "card:" + messageID + ":" + cardStatus(payload)
	return id, nil
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
	// One artifact per turn: the answer renders into the card as rich text.
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

func TestGatewayHandleQuestionAction(t *testing.T) {
	g := New(fakeProcessor{})
	done := make(chan string, 1)
	g.asks["q_1"] = &pendingAsk{openID: "ou_sender", cardID: "om_card", done: done}

	// Someone else in the chat must not answer for the person who asked.
	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_other", Action: "elicit_answer", RequestID: "q_1", Decision: "Blue", MessageID: "om_card",
	}); toast.Type != "error" {
		t.Fatalf("wrong user toast = %#v", toast)
	}
	// A button tapped on a card whose turn has moved on.
	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "elicit_answer", RequestID: "gone", Decision: "Blue", MessageID: "om_card",
	}); toast.Type != "info" {
		t.Fatalf("expired toast = %#v", toast)
	}
	// A malformed callback carries no choice at all.
	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "elicit_answer", RequestID: "q_1", MessageID: "om_card",
	}); toast.Type != "error" {
		t.Fatalf("empty choice toast = %#v", toast)
	}
	if toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", Action: "elicit_answer", RequestID: "q_1", Decision: "Blue", MessageID: "om_card",
	}); toast.Type != "success" {
		t.Fatalf("answer toast = %#v", toast)
	}
	if got := <-done; got != "Blue" {
		t.Fatalf("choice = %q, want the value the button carried", got)
	}
}

// The card reports progress, it does not stream. Growing assistant text is
// not news; a tool or a plan step changing is.
func TestCardRepaintsOnMilestonesNotOnText(t *testing.T) {
	base := card.Turn{
		Plan:  []card.Step{{Text: "one", Status: card.StepInProgress}},
		Tools: []card.Tool{{ID: "t1", Status: card.ToolRunning}},
	}
	quiet := []struct {
		name string
		next card.Progress
	}{
		{"more answer text", card.Progress{
			Answer: "a much longer answer than before",
			Plan:   base.Plan, Tools: base.Tools,
		}},
		{"more reasoning", card.Progress{
			Reasoning: "still thinking about it",
			Plan:      base.Plan, Tools: base.Tools,
		}},
		{"token counts moved", card.Progress{
			Usage: card.Usage{ContextTokens: 9000},
			Plan:  base.Plan, Tools: base.Tools,
		}},
	}
	for _, tc := range quiet {
		t.Run(tc.name, func(t *testing.T) {
			if isMilestone(base, tc.next) {
				t.Fatal("repainted the card for something the user would not act on")
			}
		})
	}

	news := []struct {
		name string
		next card.Progress
	}{
		{"plan step advanced", card.Progress{
			Plan:  []card.Step{{Text: "one", Status: card.StepCompleted}},
			Tools: base.Tools,
		}},
		{"plan step added", card.Progress{
			Plan:  append(append([]card.Step(nil), base.Plan...), card.Step{Text: "two"}),
			Tools: base.Tools,
		}},
		{"tool finished", card.Progress{
			Plan: base.Plan, Tools: []card.Tool{{ID: "t1", Status: card.ToolCompleted}},
		}},
		{"tool started", card.Progress{
			Plan:  base.Plan,
			Tools: append(append([]card.Tool(nil), base.Tools...), card.Tool{ID: "t2", Status: card.ToolRunning}),
		}},
		{"model became known", card.Progress{
			Plan: base.Plan, Tools: base.Tools,
			Settings: card.Settings{Harness: "codex", Model: "GPT 5.6 Sol"},
		}},
	}
	for _, tc := range news {
		t.Run(tc.name, func(t *testing.T) {
			if !isMilestone(base, tc.next) {
				t.Fatal("did not repaint for a change the user is waiting on")
			}
		})
	}
}

// Settings arrive once and then repeat on every snapshot; only the first is
// news, or the card would repaint forever.
func TestKnownSettingsAreNotRepeatedNews(t *testing.T) {
	settled := card.Turn{Settings: card.Settings{Harness: "codex", Model: "GPT 5.6 Sol"}}
	if isMilestone(settled, card.Progress{Settings: settled.Settings}) {
		t.Fatal("already-known settings counted as news")
	}
}

// holdingProcessor parks every call, not just the first, so a test can hold
// the pool full while it checks what still gets through.
type holdingProcessor struct {
	mu      sync.Mutex
	seen    []string
	release chan struct{}
}

func (p *holdingProcessor) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	p.mu.Lock()
	p.seen = append(p.seen, req.Input)
	p.mu.Unlock()
	<-p.release
	return turn.Result{Text: "reply: " + req.Input}, nil
}

func (p *holdingProcessor) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func TestPoolSizeTracksCPUs(t *testing.T) {
	if got, want := PoolSize(), max(2, runtime.NumCPU()*3/2); got != want {
		t.Fatalf("pool = %d, want %d", got, want)
	}
	if PoolSize() < 2 {
		t.Fatal("a single-core box must still serve more than one thing")
	}
}

// The pool bounds how many conversations run at once, so a message for a
// conversation nobody is serving waits when every slot is taken.
func TestPoolBoundsConcurrentConversations(t *testing.T) {
	processor := &holdingProcessor{release: make(chan struct{})}
	g := New(processor)
	g.BindChannel(&reply{text: make(chan string, 64)})
	g.slots = make(chan struct{}, 2)

	for i := 0; i < 4; i++ {
		g.HandleMessage(feishu.InboundMessage{
			ChatID:    fmt.Sprintf("oc_%d", i),
			MessageID: fmt.Sprintf("om_%d", i),
			Text:      fmt.Sprintf("task %d", i),
		})
	}
	deadline := time.Now().Add(time.Second)
	for len(processor.texts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	if got := processor.texts(); len(got) != 2 {
		t.Fatalf("pool of 2 admitted %d conversations: %v", len(got), got)
	}
	close(processor.release)
	deadline = time.Now().Add(2 * time.Second)
	for len(processor.texts()) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := processor.texts(); len(got) != 4 {
		t.Fatalf("waiting conversations never ran: %v", got)
	}
}

// The trap this design exists to avoid: an interrupting message must not
// queue for a slot that the turn it is interrupting is holding. With every
// slot taken, a second message for an already-served conversation still has
// to get through — otherwise the pool silently reinstates the per-chat queue
// that made interruption impossible.
func TestSaturatedPoolStillAdmitsAnInterrupt(t *testing.T) {
	processor := &holdingProcessor{release: make(chan struct{})}
	g := New(processor)
	g.BindChannel(&reply{text: make(chan string, 64)})
	g.slots = make(chan struct{}, 1)

	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_a", MessageID: "om_1", Text: "long task"})
	deadline := time.Now().Add(time.Second)
	for len(processor.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(processor.texts()) == 0 {
		t.Fatal("first message never started")
	}
	// Pool is now full and held by the very turn we want to redirect.
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_a", MessageID: "om_2", Text: "actually this"})

	deadline = time.Now().Add(2 * time.Second)
	for len(processor.texts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := processor.texts()
	if len(got) < 2 {
		t.Fatalf("interrupt blocked on a slot held by its own target: %v", got)
	}
	close(processor.release)
}

// The slot has to come back, or the pool leaks a conversation at a time.
func TestPoolReleasesSlotsAfterEveryMessage(t *testing.T) {
	processor := &blockingProcessor{release: make(chan struct{})}
	close(processor.release)
	g := New(processor)
	g.BindChannel(&reply{text: make(chan string, 64)})
	g.slots = make(chan struct{}, 1)

	for i := 0; i < 5; i++ {
		g.HandleMessage(feishu.InboundMessage{
			ChatID:    fmt.Sprintf("oc_%d", i),
			MessageID: fmt.Sprintf("om_%d", i),
			Text:      "quick",
		})
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(processor.texts()) < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := processor.texts(); len(got) != 5 {
		t.Fatalf("slot leaked: only %d of 5 ran: %v", len(got), got)
	}
	g.mu.Lock()
	inflight := len(g.serving)
	g.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("serving map leaked %d conversations", inflight)
	}
	if len(g.slots) != 0 {
		t.Fatalf("pool still holds %d slots", len(g.slots))
	}
}

// The /clear card's 恢复 button is a typing shortcut: tapping it synthesizes
// the "/history 1" the user would have written, aimed at the conversation the
// button carries — a completed turn is gone from the registry, so the card
// has to bring its own address.
func TestRecoverButtonSynthesizesHistoryRestore(t *testing.T) {
	processor := &holdingProcessor{release: make(chan struct{})}
	close(processor.release)
	g := New(processor)
	g.BindChannel(&reply{text: make(chan string, 4)})

	toast := g.HandleCardAction(feishu.CardAction{
		OpenID: "ou_sender", ChatID: "oc_chat", MessageID: "om_card",
		Action: "history_restore", RequestID: "omt_thread",
	})
	if toast.Type != "success" {
		t.Fatalf("toast = %#v", toast)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(processor.texts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := processor.texts(); len(got) != 1 || got[0] != "/history 1" {
		t.Fatalf("synthesized = %v, want [/history 1]", got)
	}
	if toast := g.HandleCardAction(feishu.CardAction{Action: "history_restore"}); toast.Type != "error" {
		t.Fatalf("empty conversation should be refused: %#v", toast)
	}
}

type threadingReply struct {
	reply
	mu      sync.Mutex
	anchors []string
	fail    bool
}

func (r *threadingReply) ReplyThread(_ context.Context, messageID, text string) (string, string, error) {
	if r.fail {
		return "", "", errors.New("no threads here")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.anchors = append(r.anchors, messageID+"::"+text)
	return "om_anchor", "omt_new", nil
}

type reqRecorder struct {
	mu   sync.Mutex
	reqs []turn.Request
}

func (p *reqRecorder) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	return turn.Result{Text: "ok"}, nil
}

// "/t 任务" seeds a topic thread and runs the task inside it: the anchor
// carries the task text, and the turn's conversation is the new thread, so
// its session is isolated from the flat chat's.
func TestTopicCommandSeedsThreadAndRoutesTask(t *testing.T) {
	processor := &reqRecorder{}
	g := New(processor)
	ch := &threadingReply{reply: reply{text: make(chan string, 4)}}
	g.BindChannel(ch)

	g.HandleMessage(feishu.InboundMessage{
		ChatID: "oc_group", MessageID: "om_ask", SenderOpenID: "ou_me",
		ChatType: "group", Mentioned: true, Text: "/t 重构登录模块",
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		processor.mu.Lock()
		n := len(processor.reqs)
		processor.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	processor.mu.Lock()
	defer processor.mu.Unlock()
	if len(processor.reqs) != 1 {
		t.Fatalf("task did not run: %+v", processor.reqs)
	}
	if processor.reqs[0].ConversationID != "omt_new" {
		t.Fatalf("conversation = %q, want the new thread", processor.reqs[0].ConversationID)
	}
	if !strings.Contains(processor.reqs[0].Input, "重构登录模块") || strings.Contains(processor.reqs[0].Input, "/t") {
		t.Fatalf("input = %q, want the bare task", processor.reqs[0].Input)
	}
	ch.mu.Lock()
	anchors := append([]string(nil), ch.anchors...)
	ch.mu.Unlock()
	if len(anchors) != 1 || anchors[0] != "om_ask::重构登录模块" {
		t.Fatalf("anchor = %v", anchors)
	}
}

// A bare /t, or one sent from inside a thread, explains itself instead of
// running anything.
func TestTopicCommandGuards(t *testing.T) {
	processor := &reqRecorder{}
	g := New(processor)
	ch := &threadingReply{reply: reply{text: make(chan string, 4)}}
	g.BindChannel(ch)

	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_group", MessageID: "om_1", Text: "/t"})
	g.HandleMessage(feishu.InboundMessage{
		ChatID: "oc_group", ConversationID: "omt_existing", MessageID: "om_2", Text: "/t 再开一个",
	})
	got := []string{<-ch.text, <-ch.text}
	for _, reply := range got {
		if !strings.Contains(reply, "任务内容") && !strings.Contains(reply, "话题") {
			t.Fatalf("guard reply = %q", reply)
		}
	}
	processor.mu.Lock()
	defer processor.mu.Unlock()
	if len(processor.reqs) != 0 {
		t.Fatalf("guarded /t still ran: %+v", processor.reqs)
	}
}

type recordingGate struct{ calls chan string }

func (g *recordingGate) Anchor(conversationID, chatID, messageID string) {
	g.calls <- conversationID + "|" + chatID + "|" + messageID
}

func (g *recordingGate) SetStyle(string, string) {}

func (g *recordingGate) Interim(string) bool { return false }

// interimGate simulates a turn during which the agent posted milestone
// cards, so the final card must land below them.
type interimGate struct{}

func (interimGate) Anchor(string, string, string) {}
func (interimGate) SetStyle(string, string)       {}
func (interimGate) Interim(string) bool           { return true }

func TestGatewayFinalCardLandsBelowInterim(t *testing.T) {
	g := New(fakeProcessor{})
	events := make(chan string, 8)
	g.BindChannel(&recordingCards{events: events})
	g.SetAgentGate(interimGate{})
	g.HandleMessage(feishu.InboundMessage{ChatID: "oc_chat", MessageID: "om_message", Text: "hello"})
	got := collectEvents(t, events, 5)
	if got[1] != "card:om_message:running" {
		t.Fatalf("start = %q", got[1])
	}
	// The answer arrives as a fresh reply (bottom of the chat), and the
	// stale opener above the milestones is recalled.
	if got[2] != "card:om_message:completed" || got[3] != "delete:om_card" {
		t.Fatalf("finish = %v", got)
	}
	if got[4] != "remove:om_message:rx_1" {
		t.Fatalf("unack = %q", got[4])
	}
}

func TestGatewayAnchorsConversationBeforeTurn(t *testing.T) {
	g := New(fakeProcessor{})
	r := &reply{text: make(chan string, 1)}
	g.BindChannel(r)
	gate := &recordingGate{calls: make(chan string, 4)}
	g.SetAgentGate(gate)
	g.HandleMessage(feishu.InboundMessage{
		ChatID: "oc_1", MessageID: "om_1", Text: "hi", ChatType: protocol.ChatP2P,
	})
	select {
	case got := <-gate.calls:
		if got != "oc_1|oc_1|om_1" {
			t.Fatalf("anchor = %q", got)
		}
	case <-time.After(waitDeadline):
		t.Fatal("gate never anchored")
	}
	select {
	case <-r.text:
	case <-time.After(waitDeadline):
		t.Fatal("turn never replied")
	}
	// A threaded message anchors its thread, not the flat chat.
	g.HandleMessage(feishu.InboundMessage{
		ChatID: "oc_1", ConversationID: "omt_thread", MessageID: "om_2", Text: "hi again", ChatType: protocol.ChatGroup, Mentioned: true,
	})
	select {
	case got := <-gate.calls:
		if got != "omt_thread|oc_1|om_2" {
			t.Fatalf("thread anchor = %q", got)
		}
	case <-time.After(waitDeadline):
		t.Fatal("gate never anchored thread")
	}
}
