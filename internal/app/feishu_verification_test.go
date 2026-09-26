package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// startupRetrySettings is the console's channel state as startup reports it.
type startupRetrySettings struct {
	mu    sync.Mutex
	retry *consoleapi.ChannelStartupRetry
}

func (s *startupRetrySettings) SetRuntimeError(string) {}
func (s *startupRetrySettings) SetStartupRetry(r *consoleapi.ChannelStartupRetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retry = r
}
func (s *startupRetrySettings) BindAccessUpdater(func(config.Feishu)) {}
func (s *startupRetrySettings) current() *consoleapi.ChannelStartupRetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retry
}

// verifiedFeishu stands in for a Feishu channel whose identity is verified:
// every text it is asked to post is delivered, to the message it answers.
type verifiedFeishu struct {
	textOnlyGatewayChannel
	mu      sync.Mutex
	answers []string
}

func (f *verifiedFeishu) ReplyText(_ context.Context, messageID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, messageID)
	return "posted-" + messageID, nil
}

func (f *verifiedFeishu) answered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.answers...)
}

// answersTo counts the answers posted to message.
func answersTo(answered []string, message string) int {
	n := 0
	for _, id := range answered {
		if id == message {
			n++
		}
	}
	return n
}

// Feishu work runs only through a verified channel. Until the channel's
// identity is verified, and for as long as its credentials are refused,
// accepted Feishu input waits unrun, a due Feishu schedule stays due and an
// agent's Feishu message is refused as unavailable: nothing runs whose
// answer could not be sent and would then never be sent. Once verified, the
// waiting input is run and answered once and the schedule fires.
func TestFeishuWorkWaitsForAVerifiedChannel(t *testing.T) {
	book, tasks := applicationGrantBook(t)
	gate := applicationGrantGate(t, book)
	deps := turntest.Deps(t, func(o *turntest.Options) { o.Ledger, o.Tasks = book, tasks })
	coordinator, err := turn.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	cons := console.New(coordinator, "owner", nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := cons.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	turns := &firingCoordinator{}
	gw := gateway.New(turns)
	ingress := &reconciliationWorkers{}
	t.Cleanup(ingress.Close)
	settings := &startupRetrySettings{}
	cfg := &config.Config{}
	cfg.Feishu.AppID, cfg.Feishu.AppSecret = "cli_unverified", "unverified-secret"
	if _, err := assembleChannels(
		&runtimeValues{book: book, cfg: cfg, ctx: t.Context()},
		&ledgerValues{attempts: attempt.New(book)},
		&executionValues{coordinator: coordinator, gw: gw, catalogText: i18n.New(i18n.LocaleEN), intents: intent.New(book)},
		&readModelValues{}, &consoleValues{cons: cons, reconciliations: ingress},
		&administrationValues{channelSettings: settings}, &delegationValues{gate: gate},
	); err != nil {
		t.Fatal(err)
	}
	settings.SetStartupRetry(&consoleapi.ChannelStartupRetry{Attempts: 1, LastError: "dial tcp: network is unreachable"})

	// Before verification.
	if err := gw.HandleMessage(feishu.InboundMessage{ConversationID: "oc_chat", ChatID: "oc_chat", MessageID: "om_waiting", SenderOpenID: "owner", ChatType: "p2p", Text: "waiting work", Mentioned: true}); err != nil {
		t.Fatalf("accepting input before verification: %v", err)
	}
	ingress.Close()
	store, err := schedule.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Minute)
	job, err := store.Create(schedule.Job{Channel: "feishu", ProjectID: "p", ConversationID: "oc_chat", ChatID: "oc_chat", ChatType: "p2p", Requester: "owner", Member: "builder", Prompt: "scheduled work", AnchorMessage: "om_schedule", Spec: schedule.Spec{Kind: schedule.KindOnce, At: at}})
	if err != nil {
		t.Fatal(err)
	}
	// fire dispatches the schedule if it is due at when.
	fire := func(when time.Time) {
		t.Helper()
		due, err := store.Due(when)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range due {
			if err := store.BeginFiring(f.Key); err != nil {
				t.Fatal(err)
			}
			dispatchFiring(t.Context(), store, nil, gw, nil, f)
		}
	}
	fire(at)
	// An agent running a task in a Feishu conversation.
	gate.Extras("chat", "agent", "agent-token", "")
	record, scope := openGrantAttempt(t, book, tasks, "agent-attempt", "ns_agent")
	if _, err := attempt.New(book).Advance(t.Context(), record.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := gate.BindExecution(t.Context(), agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	gate.Anchor("chat", channel.Address{Channel: "feishu", Conversation: "chat", Message: "om_anchor"})
	send := func() string {
		t.Helper()
		_, out := agentGateCall(t, gate, "agent-token", "tools/call", map[string]any{"name": "channel_send", "arguments": map[string]any{"content": "progress", "format": "text"}})
		return out
	}
	sent := send()
	pending, err := book.PendingCommands(t.Context(), "gateway-input")
	if err != nil {
		t.Fatal(err)
	}
	waiting, _ := store.Get(job.ID)
	if turns.calls.Load() != 0 || len(pending) != 1 || waiting.State != schedule.FiringPending ||
		!strings.Contains(sent, `channel \"feishu\" is unavailable`) || settings.current() == nil {
		t.Errorf("before verification: %d turns run, %d inputs pending, schedule %q, agent send %s, retry %+v; want no turn, the input pending, the schedule pending, Feishu unavailable and the retry shown",
			turns.calls.Load(), len(pending), waiting.State, sent, settings.current())
	}

	// Verified.
	verified := &verifiedFeishu{}
	connectFeishu(gw, gate, settings, verified)
	recovery := &reconciliationWorkers{}
	if err := gw.ReconcileQueued(t.Context(), book, coordinator, func(string, string) error { return nil }, recovery); err != nil {
		t.Fatalf("recovery after verification: %v", err)
	}
	recovery.Close()
	fire(at.Add(time.Minute))
	pending, err = book.PendingCommands(t.Context(), "gateway-input")
	if err != nil {
		t.Fatal(err)
	}
	_, scheduled := store.Get(job.ID)
	sent = send()
	answered := verified.answered()
	if turns.calls.Load() != 2 || len(pending) != 0 || scheduled || settings.current() != nil ||
		answersTo(answered, "om_waiting") != 1 || answersTo(answered, "om_schedule") != 1 || answersTo(answered, "om_anchor") != 1 {
		t.Fatalf("after verification: %d turns run, %d inputs pending, schedule still due %t, retry %+v, answered %v, agent send %s; want the input and the schedule run once each and answered, the agent's message sent, nothing pending and the retry cleared",
			turns.calls.Load(), len(pending), scheduled, settings.current(), answered, sent)
	}
}
