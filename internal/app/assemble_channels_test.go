package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// agentGateCall sends one JSON-RPC request to the agent gate as the holder
// of token and returns the HTTP status and the response body.
func agentGateCall(t *testing.T, gate *agentmcp.Server, token, method string, params any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gate.URL(), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

// assembleChannels is where the agent gate is given the Scheduler. Without
// it every channel still starts, but agents are offered no schedule tools
// and a schedule call is answered that scheduling is not wired.
func TestAssembledChannelsGiveTheAgentGateTheScheduler(t *testing.T) {
	book, tasks := applicationGrantBook(t)
	gate := applicationGrantGate(t, book)
	deps := turntest.Deps(t, func(o *turntest.Options) { o.Ledger, o.Tasks = book, tasks })
	coordinator, err := turn.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := turn.NewScheduler(turn.SchedulerDeps{Attempts: deps.Attempts, Tasks: deps.Tasks, Projects: deps.Projects, Schedules: deps.Schedules, Text: deps.Text})
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
	workers := &reconciliationWorkers{}
	t.Cleanup(workers.Close)
	if _, err := assembleChannels(
		&runtimeValues{book: book, cfg: &config.Config{}, ctx: t.Context()},
		&ledgerValues{},
		&executionValues{coordinator: coordinator, gw: gateway.New(coordinator), catalogText: i18n.New(i18n.LocaleEN), scheduler: scheduler},
		&readModelValues{}, &consoleValues{cons: cons, reconciliations: workers},
		&administrationValues{}, &delegationValues{gate: gate},
	); err != nil {
		t.Fatal(err)
	}
	gate.Extras("chat", "agent", "schedule-token", "")
	status, body := agentGateCall(t, gate, "schedule-token", "tools/list", map[string]any{})
	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &listed); status != http.StatusOK || err != nil {
		t.Fatalf("tools/list: %d %v %s", status, err, body)
	}
	var offered []string
	for _, tool := range listed.Result.Tools {
		offered = append(offered, tool.Name)
	}
	for _, name := range []string{"steve_schedule", "steve_schedules", "steve_schedule_cancel"} {
		if !slices.Contains(offered, name) {
			t.Fatalf("assembled agent gate offers %v, without %s", offered, name)
		}
	}
	record, scope := openGrantAttempt(t, book, tasks, "schedule-attempt", "ns_schedule")
	if _, err := attempt.New(book).Advance(t.Context(), record.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := gate.BindExecution(t.Context(), agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	// The execution is not a complete owner schedule identity, so the call
	// reaches the Scheduler and the Scheduler refuses it.
	status, out := agentGateCall(t, gate, "schedule-token", "tools/call", map[string]any{"name": "steve_schedules", "arguments": map[string]any{}})
	if status != http.StatusOK || !strings.Contains(out, agentmcp.ErrGrantDenied.Error()) {
		t.Fatalf("steve_schedules was not answered by the Scheduler: %d %s", status, out)
	}
}

// unsettledIngressTurn admits the attempt it is given and returns a result
// without it, as a turn that does not settle the attempt it admitted.
type unsettledIngressTurn struct {
	turntest.IdleCoordinator
	task, attempt string
	calls         atomic.Int32
}

func (u *unsettledIngressTurn) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	u.calls.Add(1)
	req.OnTurnReady(u.task, u.attempt)
	return turn.Result{Text: "unsettled result"}, nil
}

// assembleChannels hands ingress the coordinator as its recovery driver.
// An accepted Feishu input whose turn leaves its admitted attempt unsettled
// is then recovered while ingress still handles it: the attempt's committed
// answer is delivered and accounted, and no reconciler pass is needed.
func TestAssembledIngressRecoversAnUnsettledTurnThroughTheCoordinator(t *testing.T) {
	f := openCrashProbe(t, t.TempDir())
	t.Cleanup(func() { f.close(t) })
	r := f.seed(t, true, "", "feishu")
	unsettled := &unsettledIngressTurn{task: r.TaskID, attempt: r.ID}
	g := gateway.New(unsettled)
	ch := &crashGatewayChannel{}
	g.BindChannel(ch)
	workers := &reconciliationWorkers{}
	t.Cleanup(workers.Close)
	if _, err := assembleChannels(
		&runtimeValues{book: f.book, cfg: &config.Config{}, ctx: f.ctx},
		&ledgerValues{},
		&executionValues{coordinator: f.c, gw: g, catalogText: i18n.New(i18n.LocaleEN)},
		&readModelValues{}, &consoleValues{cons: f.cons, reconciliations: workers},
		&administrationValues{}, &delegationValues{},
	); err != nil {
		t.Fatal(err)
	}
	if err := g.HandleMessage(feishu.InboundMessage{
		ConversationID: crashConversation, ChatID: "console", MessageID: "web-original",
		SenderOpenID: "owner", Text: "original goal", Mentioned: true, ChatType: protocol.ChatP2P,
	}); err != nil {
		t.Fatal(err)
	}
	workers.Close()
	pending, err := f.book.PendingCommands(f.ctx, "gateway-input")
	if err != nil {
		t.Fatal(err)
	}
	tracked, found := f.tasks.Get(r.TaskID)
	if !found {
		t.Fatalf("task %s is gone", r.TaskID)
	}
	if len(pending) != 0 || ch.replies.Load() != 1 || unsettled.calls.Load() != 1 ||
		tracked.Budget.Tokens.Total != 18 || tracked.Attempts[0].Open() {
		t.Fatalf("ingress left the unsettled turn to the reconciler: pending=%d replies=%d turns=%d tokens=%d attempt open=%v",
			len(pending), ch.replies.Load(), unsettled.calls.Load(), tracked.Budget.Tokens.Total, tracked.Attempts[0].Open())
	}
}
