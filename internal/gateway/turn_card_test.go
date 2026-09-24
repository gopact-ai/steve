package gateway

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gopact-ai/steve/internal/card"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

func TestCardProgressChangeDetection(t *testing.T) {
	snapshot := func() card.Progress {
		return card.Progress{
			Answer: "answer", Reasoning: "thinking",
			Plan: []card.Step{{Text: "inspect", Status: card.StepInProgress}},
			Tools: []card.Tool{{
				ID: "parent", Kind: "execute", Name: "command", Detail: "detail",
				Input: "input", Output: "output", Status: card.ToolRunning,
				StartedAt: time.Unix(100, 0), UpdatedAt: time.Unix(101, 0),
				Children: []card.Tool{{
					ID: "child", Status: card.ToolRunning,
					Children: []card.Tool{{ID: "grandchild", Status: card.ToolRunning}},
				}},
			}},
			Usage:    card.Usage{OutputTokens: 10, Cost: &view.Cost{Amount: 1, Currency: "USD"}},
			Settings: card.Settings{Harness: "codex", Model: "model", Mode: "agent"},
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*card.Progress)
		want   bool
	}{
		{"identical snapshot including separately allocated cost and children", func(*card.Progress) {}, false},
		{"answer", func(p *card.Progress) { p.Answer += " more" }, true},
		{"answer cleared", func(p *card.Progress) { p.Answer = "" }, true},
		{"reasoning", func(p *card.Progress) { p.Reasoning += " more" }, true},
		{"tool kind", func(p *card.Progress) { p.Tools[0].Kind = "read" }, true},
		{"tool name", func(p *card.Progress) { p.Tools[0].Name += " more" }, true},
		{"tool detail", func(p *card.Progress) { p.Tools[0].Detail += " more" }, true},
		{"tool input", func(p *card.Progress) { p.Tools[0].Input += " more" }, true},
		{"tool output", func(p *card.Progress) { p.Tools[0].Output += " more" }, true},
		{"tool start", func(p *card.Progress) { p.Tools[0].StartedAt = time.Unix(99, 0) }, true},
		{"tool update", func(p *card.Progress) { p.Tools[0].UpdatedAt = time.Unix(102, 0) }, true},
		{"child output", func(p *card.Progress) { p.Tools[0].Children[0].Output = "child output" }, true},
		{"child status", func(p *card.Progress) { p.Tools[0].Children[0].Status = card.ToolCompleted }, true},
		{"grandchild output", func(p *card.Progress) { p.Tools[0].Children[0].Children[0].Output = "nested output" }, true},
		{"child added", func(p *card.Progress) { p.Tools[0].Children = append(p.Tools[0].Children, card.Tool{ID: "new"}) }, true},
		{"child removed", func(p *card.Progress) { p.Tools[0].Children = nil }, true},
		{"empty children", func(p *card.Progress) { p.Tools[0].Children[0].Children[0].Children = []card.Tool{} }, false},
		{"usage", func(p *card.Progress) { p.Usage.OutputTokens++ }, true},
		{"usage cost", func(p *card.Progress) { p.Usage.Cost.Amount++ }, true},
		{"usage cost currency", func(p *card.Progress) { p.Usage.Cost.Currency = "EUR" }, true},
		{"usage report", func(p *card.Progress) { p.Usage.Reported = true }, true},
		{"context usage", func(p *card.Progress) { p.Usage.ContextTokens++ }, true},
		{"harness changed", func(p *card.Progress) { p.Settings.Harness = "other" }, true},
		{"model changed", func(p *card.Progress) { p.Settings.Model = "other" }, true},
		{"mode changed", func(p *card.Progress) { p.Settings.Mode = "read-only" }, true},
		{"missing settings retains identity", func(p *card.Progress) { p.Settings = card.Settings{} }, false},
		{"invisible settings", func(p *card.Progress) {
			p.Settings.Adapter = "adapter version"
			p.Settings.Models = []string{"other"}
			p.Settings.Options = []view.Option{{ID: "effort", Current: "high"}}
			p.Settings.Node = "remote"
		}, false},
		{"settings whitespace", func(p *card.Progress) { p.Settings.Model = " model " }, false},
		{"non-card metadata", func(p *card.Progress) {
			p.Agent = "worker"
			p.Timeline = []view.Span{{Kind: "text", Text: "answer"}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := snapshot()
			state := card.Turn{
				Answer: base.Answer, Reasoning: base.Reasoning, Plan: base.Plan,
				Tools: base.Tools, Usage: base.Usage, Settings: base.Settings,
			}
			next := snapshot()
			tc.change(&next)
			if got := isMilestone(state, next); got != tc.want {
				t.Fatalf("isMilestone = %v, want %v", got, tc.want)
			}
		})
	}
}

// All I/O stays in memory. synctest advances the real three-second scheduler
// without sleeping in wall time or changing a package-global interval.
type progressCards struct {
	nopChannel
	patches chan []byte
}

func (*progressCards) Reply(context.Context, string, string) error { return nil }
func (*progressCards) ReplyCard(context.Context, string, []byte) (string, error) {
	return "progress-card", nil
}
func (c *progressCards) PatchCard(_ context.Context, _ string, payload []byte) error {
	c.patches <- append([]byte(nil), payload...)
	return nil
}

type streamingCardProcessor struct {
	requests chan turn.Request
	release  chan struct{}
}

func (p *streamingCardProcessor) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	p.requests <- req
	<-p.release
	return turn.Result{Text: "finished answer"}, nil
}

func expectNoCardPatch(t *testing.T, ch *progressCards) {
	t.Helper()
	select {
	case payload := <-ch.patches:
		t.Fatalf("unexpected patch: %s", payload)
	default:
	}
}

func takeCardPatch(t *testing.T, ch *progressCards, text string) []byte {
	t.Helper()
	select {
	case payload := <-ch.patches:
		if !strings.Contains(string(payload), text) {
			t.Fatalf("patch missing %q: %s", text, payload)
		}
		return payload
	default:
		t.Fatalf("no patch containing %q", text)
		return nil
	}
}

func TestGatewayPureTextPatchesBeforeFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &streamingCardProcessor{requests: make(chan turn.Request, 1), release: make(chan struct{})}
		g := New(p)
		ch := &progressCards{patches: make(chan []byte, 16)}
		g.BindChannel(ch)
		done := make(chan error, 1)
		go func() {
			done <- g.process(feishu.InboundMessage{ChatID: "chat", MessageID: "message", Text: "hello"})
		}()
		req := <-p.requests
		defer func() {
			close(p.release)
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()

		req.OnProgress(card.Progress{Answer: "first chunk"})
		time.Sleep(time.Second)
		req.OnProgress(card.Progress{Answer: "latest text before finish"})
		time.Sleep(2*time.Second - time.Nanosecond)
		synctest.Wait()
		expectNoCardPatch(t, ch)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		payload := takeCardPatch(t, ch, "latest text before finish")
		if !strings.Contains(string(payload), "turn_cancel") {
			t.Fatalf("progress patch lost running-turn controls: %s", payload)
		}
		select {
		case err := <-done:
			t.Fatalf("processor finished before the progress patch: %v", err)
		default:
		}

		// An identical snapshot and elapsed time alone must not rearm a timer.
		req.OnProgress(card.Progress{Answer: "latest text before finish"})
		time.Sleep(2 * cardMinInterval)
		synctest.Wait()
		expectNoCardPatch(t, ch)

		req.OnProgress(card.Progress{Answer: "next burst"})
		synctest.Wait()
		takeCardPatch(t, ch, "next burst")
		req.OnProgress(card.Progress{Answer: "another chunk"})
		time.Sleep(time.Second)
		req.OnProgress(card.Progress{Answer: "latest chunk in burst"})
		time.Sleep(2*time.Second - time.Nanosecond)
		synctest.Wait()
		expectNoCardPatch(t, ch)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		takeCardPatch(t, ch, "latest chunk in burst")
	})
}

func TestTurnCardSettingsChangesRetainKnownIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := New(fakeProcessor{})
		ch := &progressCards{patches: make(chan []byte, 16)}
		g.BindChannel(ch)
		ui := g.newTurnUI(feishu.InboundMessage{ChatID: "chat", MessageID: "message"}, false)
		defer ui.closeProgress()
		p := card.Progress{Settings: card.Settings{Harness: "codex", Model: "first-model", Mode: "agent"}}
		ui.progress(p)
		time.Sleep(cardMinInterval)
		synctest.Wait()
		takeCardPatch(t, ch, "first-model")

		p.Settings.Model = "second-model"
		ui.progress(p)
		// A snapshot without settings must not erase a pending identity update.
		ui.progress(card.Progress{})
		synctest.Wait()
		expectNoCardPatch(t, ch)
		time.Sleep(cardMinInterval)
		synctest.Wait()
		takeCardPatch(t, ch, "second-model")

		p.Settings.Models = []string{"third-model"}
		ui.progress(p)
		time.Sleep(2 * cardMinInterval)
		synctest.Wait()
		expectNoCardPatch(t, ch)
	})
}

func TestTurnCardUsageRefreshesOnlyOnChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := New(fakeProcessor{})
		ch := &progressCards{patches: make(chan []byte, 16)}
		g.BindChannel(ch)
		ui := g.newTurnUI(feishu.InboundMessage{ChatID: "chat", MessageID: "message"}, false)
		defer ui.closeProgress()
		p := card.Progress{Usage: card.Usage{OutputTokens: 10}}
		ui.progress(p)
		time.Sleep(cardMinInterval)
		synctest.Wait()
		takeCardPatch(t, ch, `"schema":"2.0"`)
		ui.progress(p)
		time.Sleep(2 * cardMinInterval)
		synctest.Wait()
		expectNoCardPatch(t, ch)
		p.Usage.OutputTokens++
		ui.progress(p)
		synctest.Wait()
		takeCardPatch(t, ch, `"schema":"2.0"`)
	})
}

func TestTurnCardFinishCancelsPendingProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"completed", nil, "final answer"},
		{"cancelled", context.Canceled, "turn_retry"},
		{"failed", errors.New("failed"), "turn_retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := New(fakeProcessor{})
				ch := &progressCards{patches: make(chan []byte, 16)}
				g.BindChannel(ch)
				ui := g.newTurnUI(feishu.InboundMessage{ChatID: "chat", MessageID: "message"}, false)
				defer ui.closeProgress()
				ui.progress(card.Progress{Answer: "unfinished"})
				id, err := ui.finish(turn.Result{Text: "final answer"}, tc.err)
				if err != nil || id != "progress-card" {
					t.Fatalf("finish receipt = %q, %v", id, err)
				}
				takeCardPatch(t, ch, tc.want)
				ui.progress(card.Progress{Answer: "late snapshot"})
				time.Sleep(2 * cardMinInterval)
				synctest.Wait()
				expectNoCardPatch(t, ch)
			})
		})
	}
}

func TestTurnCardApprovalFlushesPendingTextImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := New(fakeProcessor{})
		ch := &progressCards{patches: make(chan []byte, 16)}
		g.BindChannel(ch)
		ui := g.newTurnUI(feishu.InboundMessage{ChatID: "chat", MessageID: "message"}, false)
		defer ui.closeProgress()
		ui.progress(card.Progress{Answer: "pending answer"})
		ui.setApproval(&card.Approval{RequestID: "approval", ToolName: "execute"})
		payload := takeCardPatch(t, ch, "pending answer")
		if !strings.Contains(string(payload), "tool_approval") {
			t.Fatalf("approval controls missing: %s", payload)
		}
		time.Sleep(2 * cardMinInterval)
		synctest.Wait()
		expectNoCardPatch(t, ch)
		ui.setApproval(nil)
		takeCardPatch(t, ch, "turn_cancel")
	})
}

func TestTurnCardLocalizedPhasesSurviveProgress(t *testing.T) {
	for _, locale := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		t.Run(string(locale), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := New(fakeProcessor{})
				g.SetCatalog(i18n.New(locale))
				ch := &progressCards{patches: make(chan []byte, 16)}
				g.BindChannel(ch)
				msg := feishu.InboundMessage{ChatID: "chat", MessageID: "message"}
				ui := g.newTurnUI(msg, false)
				defer ui.closeProgress()
				if ui.copy.Finishing != g.text.T(i18n.CardFinishing) || ui.copy.Saving != g.text.T(i18n.CardSaving) ||
					ui.copy.Partial != g.text.T(i18n.CardPartial) {
					t.Fatalf("card copy not localized: finishing=%q saving=%q partial=%q", ui.copy.Finishing, ui.copy.Saving, ui.copy.Partial)
				}
				req := g.taskRequest(msg, "", ui)
				for _, phase := range []struct {
					value view.Phase
					key   i18n.Key
				}{
					{card.PhaseRunning, i18n.CardRunning},
					{view.PhaseFinishing, i18n.CardFinishing},
					{view.PhaseSaving, i18n.CardSaving},
				} {
					req.OnPhase(phase.value)
					req.OnProgress(card.Progress{Answer: "visible progress"})
					time.Sleep(cardMinInterval)
					synctest.Wait()
					takeCardPatch(t, ch, g.text.T(phase.key))
					ui.mu.Lock()
					got := ui.state.Phase
					ui.mu.Unlock()
					if got != phase.value {
						t.Fatalf("progress changed explicit phase %q to %q", phase.value, got)
					}
				}
			})
		})
	}
}

func TestTurnCardSubstantiveProgressLeavesWaking(t *testing.T) {
	for _, tc := range []struct {
		name string
		next card.Progress
		want card.Phase
	}{
		{"answer", card.Progress{Answer: "working answer"}, card.PhaseRunning},
		{"reasoning", card.Progress{Reasoning: "working reasoning"}, card.PhaseRunning},
		{"tools", card.Progress{Tools: []card.Tool{{ID: "tool", Status: card.ToolRunning}}}, card.PhaseRunning},
		{"plan", card.Progress{Plan: []card.Step{{Text: "working step", Status: card.StepInProgress}}}, card.PhaseRunning},
		{"settings only", card.Progress{Settings: card.Settings{Model: "model"}}, card.PhaseWaking},
		{"usage only", card.Progress{Usage: card.Usage{OutputTokens: 10}}, card.PhaseWaking},
		{"empty", card.Progress{}, card.PhaseWaking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := New(fakeProcessor{})
				ch := &progressCards{patches: make(chan []byte, 16)}
				g.BindChannel(ch)
				ui := g.newTurnUI(feishu.InboundMessage{ChatID: "chat", MessageID: "message"}, false)
				defer ui.closeProgress()
				ui.progress(tc.next)
				ui.mu.Lock()
				got := ui.state.Phase
				ui.mu.Unlock()
				if got != tc.want {
					t.Fatalf("phase = %q, want %q", got, tc.want)
				}
				if tc.want == card.PhaseRunning {
					time.Sleep(cardMinInterval)
					synctest.Wait()
					payload := takeCardPatch(t, ch, ui.copy.Running)
					if strings.Contains(string(payload), ui.copy.Waking) {
						t.Fatalf("substantive progress still renders waking: %s", payload)
					}
				}
			})
		})
	}
}

// A callback already fired before an immediate control patch can arrive at
// flush late. It must check the latest send time instead of bypassing the floor.
func TestTurnCardOrdinaryFlushRechecksInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := New(fakeProcessor{})
		ch := &progressCards{patches: make(chan []byte, 16)}
		g.BindChannel(ch)
		ui := g.newTurnUI(feishu.InboundMessage{ChatID: "chat", MessageID: "message"}, false)
		defer ui.closeProgress()
		ui.setApproval(&card.Approval{RequestID: "approval"})
		takeCardPatch(t, ch, "tool_approval")
		ui.progress(card.Progress{Answer: "new text"})
		ui.flush()
		expectNoCardPatch(t, ch)
		time.Sleep(cardMinInterval)
		synctest.Wait()
		takeCardPatch(t, ch, "new text")
		time.Sleep(2 * cardMinInterval)
		synctest.Wait()
		expectNoCardPatch(t, ch)
	})
}

type blockedCardPatch struct {
	payload []byte
	release chan error
}

type blockedProgressCards struct {
	progressCards
	started   chan *blockedCardPatch
	completed chan []byte
	replies   chan string
	abort     chan struct{}
}

func (c *blockedProgressCards) Reply(_ context.Context, _ string, text string) error {
	c.replies <- text
	return nil
}

func (c *blockedProgressCards) PatchCard(ctx context.Context, _ string, payload []byte) error {
	call := &blockedCardPatch{payload: append([]byte(nil), payload...), release: make(chan error, 1)}
	c.started <- call
	var err error
	select {
	case err = <-call.release:
	case <-c.abort:
	case <-ctx.Done():
		err = ctx.Err()
	}
	c.completed <- call.payload
	return err
}

func newBlockedCardUI(t *testing.T) (*turnUI, *blockedProgressCards) {
	t.Helper()
	g := New(fakeProcessor{})
	ch := &blockedProgressCards{
		started: make(chan *blockedCardPatch, 16), completed: make(chan []byte, 16),
		replies: make(chan string, 16), abort: make(chan struct{}),
	}
	g.BindChannel(ch)
	ui := g.newTurnUI(feishu.InboundMessage{ChatID: "chat", MessageID: "message"}, false)
	t.Cleanup(func() {
		close(ch.abort)
		ui.closeProgress()
	})
	return ui, ch
}

func nextBlockedCardPatch(t *testing.T, ch *blockedProgressCards) *blockedCardPatch {
	t.Helper()
	select {
	case call := <-ch.started:
		return call
	case <-time.After(waitDeadline):
		t.Fatal("timed out waiting for card patch")
		return nil
	}
}

func expectNoBlockedCardPatch(t *testing.T, ch *blockedProgressCards) {
	t.Helper()
	select {
	case call := <-ch.started:
		t.Fatalf("patch overtook the blocked send: %s", call.payload)
	case <-time.After(20 * time.Millisecond):
	}
}

// Poll only under the state lock: reaching this condition while a network
// call is blocked also proves the sender does not hold that lock over I/O.
func waitCardState(t *testing.T, ui *turnUI, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for time.Now().Before(deadline) {
		ui.mu.Lock()
		ok := ready()
		ui.mu.Unlock()
		if ok {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("timed out waiting for card state")
}

func makeCardPatchDue(ui *turnUI) {
	ui.mu.Lock()
	ui.lastPatch = time.Now().Add(-cardMinInterval)
	ui.mu.Unlock()
}

func TestTurnCardSerializesBlockedProgressAndControls(t *testing.T) {
	for _, kind := range []string{"ordinary", "approval", "question"} {
		t.Run(kind, func(t *testing.T) {
			ui, ch := newBlockedCardUI(t)
			makeCardPatchDue(ui)
			ui.progress(card.Progress{Answer: "old text"})
			first := nextBlockedCardPatch(t, ch)
			changed := make(chan struct{})
			wantControl := ""
			switch kind {
			case "ordinary":
				makeCardPatchDue(ui)
				ui.progress(card.Progress{Answer: "queued text"})
				close(changed)
			case "approval":
				wantControl = "tool_approval"
				go func() {
					ui.setApproval(&card.Approval{RequestID: "approval"})
					close(changed)
				}()
				waitCardState(t, ui, func() bool { return ui.state.Approval != nil })
			case "question":
				wantControl = "elicit_answer"
				go func() {
					ui.setQuestion(&card.Question{RequestID: "question", Message: "choose", Choices: []card.Choice{{Value: "yes", Label: "Yes"}}})
					close(changed)
				}()
				waitCardState(t, ui, func() bool { return ui.state.Question != nil })
			}
			expectNoBlockedCardPatch(t, ch)
			// Queued sends must render after acquiring the send lock, not
			// retain a payload captured while the previous send was blocked.
			ui.progress(card.Progress{Answer: "newest text"})
			first.release <- nil
			second := nextBlockedCardPatch(t, ch)
			if !strings.Contains(string(second.payload), "newest text") ||
				!strings.Contains(string(second.payload), wantControl) {
				t.Fatalf("queued patch did not use latest state: %s", second.payload)
			}
			select {
			case <-ch.completed:
			default:
				t.Fatal("second patch began before the first completed")
			}
			second.release <- nil
			select {
			case <-changed:
			case <-time.After(waitDeadline):
				t.Fatal("control update did not return")
			}
			ui.closeProgress()
			expectNoBlockedCardPatch(t, ch)
		})
	}
}

func TestTurnCardFinalAndCloseWaitForBlockedPatch(t *testing.T) {
	for _, finish := range []bool{false, true} {
		name := "close"
		if finish {
			name = "finish"
		}
		t.Run(name, func(t *testing.T) {
			ui, ch := newBlockedCardUI(t)
			makeCardPatchDue(ui)
			ui.progress(card.Progress{Answer: "old text"})
			first := nextBlockedCardPatch(t, ch)
			controlDone := make(chan struct{})
			go func() {
				ui.setApproval(&card.Approval{RequestID: "queued-approval"})
				close(controlDone)
			}()
			waitCardState(t, ui, func() bool { return ui.state.Approval != nil })
			done := make(chan error, 1)
			go func() {
				if !finish {
					ui.closeProgress()
					done <- nil
					return
				}
				id, err := ui.finish(turn.Result{Text: "final text"}, nil)
				if err == nil && id != "progress-card" {
					err = errors.New("final card lost receipt")
				}
				done <- err
			}()
			waitCardState(t, ui, func() bool { return ui.closed })
			expectNoBlockedCardPatch(t, ch)
			select {
			case err := <-done:
				t.Fatalf("closed before in-flight patch returned: %v", err)
			default:
			}
			first.release <- nil
			var finalDrained chan struct{}
			if finish {
				final := nextBlockedCardPatch(t, ch)
				if !strings.Contains(string(final.payload), "final text") ||
					strings.Contains(string(final.payload), "tool_approval") ||
					strings.Contains(string(final.payload), "turn_cancel") {
					t.Fatalf("final patch contains stale progress or controls: %s", final.payload)
				}
				select {
				case err := <-done:
					t.Fatalf("finish returned before final delivery completed: %v", err)
				default:
				}
				ui.progress(card.Progress{Answer: "late text during final"})
				waitCardState(t, ui, func() bool { return ui.state.Answer == "final text" })
				finalDrained = make(chan struct{})
				go func() {
					ui.closeProgress()
					close(finalDrained)
				}()
				select {
				case <-finalDrained:
					t.Fatal("close returned while final delivery was still blocked")
				case <-time.After(20 * time.Millisecond):
				}
				final.release <- nil
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(waitDeadline):
				t.Fatal("final/close failed to drain progress")
			}
			if finalDrained != nil {
				select {
				case <-finalDrained:
				case <-time.After(waitDeadline):
					t.Fatal("close failed to drain final delivery")
				}
			}
			select {
			case <-controlDone:
			case <-time.After(waitDeadline):
				t.Fatal("queued approval did not return")
			}
			ui.progress(card.Progress{Answer: "late text"})
			ui.setApproval(&card.Approval{RequestID: "late-approval"})
			ui.setQuestion(&card.Question{RequestID: "late-question"})
			ui.flush()
			expectNoBlockedCardPatch(t, ch)
		})
	}
}

func TestTurnCardFinalObservesBlockedPatchFailure(t *testing.T) {
	ui, ch := newBlockedCardUI(t)
	makeCardPatchDue(ui)
	ui.progress(card.Progress{Answer: "old text"})
	first := nextBlockedCardPatch(t, ch)
	done := make(chan error, 1)
	go func() {
		id, err := ui.finish(turn.Result{Text: "final text", Activity: []string{"tool output"}}, nil)
		if err == nil && id != "reply:message" {
			err = errors.New("text fallback lost receipt")
		}
		done <- err
	}()
	waitCardState(t, ui, func() bool { return ui.closed })
	first.release <- errors.New("patch denied")
	select {
	case text := <-ch.replies:
		if !strings.Contains(text, "final text") || !strings.Contains(text, "tool output") {
			t.Fatalf("final text ignored settled fallback state: %q", text)
		}
	case call := <-ch.started:
		t.Fatalf("final retried a card already in fallback: %s", call.payload)
	case <-time.After(waitDeadline):
		t.Fatal("final delivery did not observe in-flight patch failure")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("fallback finish did not return")
	}
	expectNoBlockedCardPatch(t, ch)
}
