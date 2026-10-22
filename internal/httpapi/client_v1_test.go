package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/clientapi"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

func v1Call(t *testing.T, server *Server, method, path, body string, want int) []byte {
	t.Helper()
	code, _, raw := taskMetaRequest(t, server, method, path, testToken, body)
	if code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, code, want, raw)
	}
	return raw
}

func v1Decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func v1ErrorCode(t *testing.T, raw []byte) string {
	t.Helper()
	return v1Decode[clientapi.Error](t, raw).Error.Code
}

// v1 sits behind the console's token and says what this server serves.
func TestClientV1IsGuardedAndInitializes(t *testing.T) {
	server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	for _, path := range []string{"/v1/initialize", "/v1/threads", "/v1/events"} {
		if code, _, _ := taskMetaRequest(t, server, http.MethodGet, path, "", ""); code != http.StatusUnauthorized {
			t.Fatalf("%s without token = %d", path, code)
		}
		if code, _, _ := taskMetaRequest(t, server, http.MethodGet, path, "wrong", ""); code != http.StatusUnauthorized {
			t.Fatalf("%s with a wrong token = %d", path, code)
		}
	}
	if code := v1ErrorCode(t, v1Call(t, server, http.MethodGet, "/v1/threads", "", http.StatusNotImplemented)); code != clientapi.CodeNotImplemented {
		t.Fatalf("unwired console code = %s", code)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL()+"/v1/initialize", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Accept-Language", "zh-CN")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var init clientapi.Initialize
	if err := json.NewDecoder(res.Body).Decode(&init); err != nil {
		t.Fatal(err)
	}
	if init.Protocol != "steve.client.v1" || init.ServerVersion == "" || init.Locale != "zh" || strings.Contains(strings.Join(init.Capabilities, ","), "turns") {
		t.Fatalf("initialize without a console = %+v", init)
	}
	server.SetConsole(&fakeConsole{})
	init = v1Decode[clientapi.Initialize](t, v1Call(t, server, http.MethodGet, "/v1/initialize", "", http.StatusOK))
	if !strings.Contains(strings.Join(init.Capabilities, ","), "turns") {
		t.Fatalf("initialize with a console = %+v", init)
	}
}

// A command_id names one turn whichever route submitted it: v1 replays the
// console queue's receipt, and both refuse the ID with other input.
func TestClientV1TurnCommandIDIsTheConsoleSubmissionKey(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	handler := &submissionHandler{}
	service := console.New(handler, "owner", nil)
	if err := service.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	server.SetConsole(service)
	first := v1Decode[clientapi.Turn](t, v1Call(t, server, http.MethodPost, "/v1/turns", `{"thread":"console:v1","input":"hello","command_id":"key"}`, http.StatusOK))
	if first.ID == "" || first.Thread != "console:v1" || first.CommandID != "key" {
		t.Fatalf("turn = %+v", first)
	}
	replay := v1Decode[clientapi.Turn](t, v1Call(t, server, http.MethodPost, "/v1/turns", `{"thread":"v1","input":" hello ","command_id":"key"}`, http.StatusOK))
	queued := v1Decode[consoleapi.Exchange](t, v1Call(t, server, http.MethodPost, "/console/queue", `{"conversation":"console:v1","input":"hello","command_id":"key"}`, http.StatusOK))
	if replay.ID != first.ID || queued.ID != first.ID {
		t.Fatalf("one command made several turns: v1 %s, replay %s, console %s", first.ID, replay.ID, queued.ID)
	}
	if code := v1ErrorCode(t, v1Call(t, server, http.MethodPost, "/v1/turns", `{"thread":"console:v1","input":"changed","command_id":"key"}`, http.StatusConflict)); code != clientapi.CodeCommandConflict {
		t.Fatalf("conflict code = %s", code)
	}
	v1Call(t, server, http.MethodPost, "/console/queue", `{"conversation":"console:v1","input":"changed","command_id":"key"}`, http.StatusConflict)
	v1Call(t, server, http.MethodPost, "/v1/turns", `{"thread":"console:v1","input":"hello"}`, http.StatusBadRequest)
	v1Call(t, server, http.MethodPost, "/v1/turns", `{"thread":"console:v1","input":"hello","command_id":"k2","unknown":1}`, http.StatusBadRequest)
	deadline := time.Now().Add(5 * time.Second)
	for {
		detail := v1Decode[clientapi.ThreadDetail](t, v1Call(t, server, http.MethodGet, "/v1/threads/console:v1", "", http.StatusOK))
		var user, assistant bool
		for _, item := range detail.Items {
			user = user || item.Kind == clientapi.ItemMessage && item.Role == "user" && item.Text == "hello" && item.Turn == first.ID
			assistant = assistant || item.Kind == clientapi.ItemMessage && item.Role == "assistant" && item.Text == "handled hello" && item.Turn == first.ID
		}
		if user && assistant {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread never showed the exchange: %+v", detail)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls := handler.calls.Load(); calls != 1 {
		t.Fatalf("the command ran %d times", calls)
	}
}

// boundHandler binds every conversation to project alpha.
type boundHandler struct {
	turntest.IdleCoordinator
	calls atomic.Int32
}

func (h *boundHandler) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	h.calls.Add(1)
	return turn.Result{Text: "handled " + req.Input}, nil
}

func (h *boundHandler) Context(_ context.Context, conversation string) (turn.Context, error) {
	return turn.Context{Conversation: conversation, Project: &turn.ContextProject{ID: "alpha", Bound: true}}, nil
}

// A turn expecting another project than the thread's is refused as the
// console refuses it, before anything runs; v1 names it with 409.
func TestClientV1ExpectedProjectConflict(t *testing.T) {
	handler := &boundHandler{}
	server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	server.SetConsole(console.New(handler, "owner", nil))
	raw := v1Call(t, server, http.MethodPost, "/v1/turns", `{"thread":"console:p","input":"go","command_id":"c1","expected_project":"beta"}`, http.StatusConflict)
	if e := v1Decode[clientapi.Error](t, raw).Error; e.Code != clientapi.CodeProjectConflict || !strings.Contains(e.Message, "alpha") {
		t.Fatalf("conflict = %+v", e)
	}
	if code, _, raw := taskMetaRequest(t, server, http.MethodPost, "/console/queue", testToken, `{"conversation":"console:p","input":"go","command_id":"c2","expected_project":"beta"}`); code < 400 || !strings.Contains(string(raw), "alpha") {
		t.Fatalf("the console accepted the mismatch: %d %s", code, raw)
	}
	if handler.calls.Load() != 0 {
		t.Fatal("a refused turn ran")
	}
	accepted := v1Decode[clientapi.Turn](t, v1Call(t, server, http.MethodPost, "/v1/turns", `{"thread":"console:p","input":"go","command_id":"c3","expected_project":"alpha"}`, http.StatusOK))
	if accepted.ExpectedProject != "alpha" {
		t.Fatalf("accepted turn = %+v", accepted)
	}
}

// Steer and interrupt act on queued turns the way the console queue does,
// and stay away from threads that belong to a channel.
func TestClientV1SteerAndInterruptQueuedTurns(t *testing.T) {
	f := &fakeConsole{exchanges: []consoleapi.Exchange{{ID: "1", Conversation: "console:main", Input: "one", State: "running"}, {ID: "2", Conversation: "console:main", Input: "two", State: "queued"}, {ID: "3", Conversation: "console:main", Input: "three", State: "queued"}, {ID: "4", Conversation: "console:main", Input: "four", State: "done"}}}
	server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	server.SetConsole(f)
	steered := v1Decode[clientapi.Turn](t, v1Call(t, server, http.MethodPost, "/v1/turns/2/steer", "", http.StatusOK))
	if steered.State != clientapi.TurnRunning || steered.Input != "!two" {
		t.Fatalf("steered = %+v", steered)
	}
	if code := v1ErrorCode(t, v1Call(t, server, http.MethodPost, "/v1/turns/2/steer", "", http.StatusConflict)); code != clientapi.CodeConflict {
		t.Fatalf("steering a started turn = %s", code)
	}
	interrupted := v1Decode[clientapi.InterruptResult](t, v1Call(t, server, http.MethodPost, "/v1/turns/3/interrupt", `{"command_id":"stop-3"}`, http.StatusOK))
	if interrupted.Turn.State != clientapi.TurnCancelled || interrupted.Control != nil || len(f.Queue("console:main")) != 3 {
		t.Fatalf("interrupting a queued turn = %+v, queue %+v", interrupted, f.exchanges)
	}
	stopped := v1Decode[clientapi.InterruptResult](t, v1Call(t, server, http.MethodPost, "/v1/turns/1/interrupt", `{"command_id":"stop-1"}`, http.StatusOK))
	if stopped.Control == nil || stopped.Control.Input != "/cancel" || stopped.Control.CommandID != "stop-1" {
		t.Fatalf("interrupting a running turn = %+v", stopped)
	}
	v1Call(t, server, http.MethodPost, "/v1/turns/4/interrupt", `{"command_id":"late"}`, http.StatusConflict)
	v1Call(t, server, http.MethodPost, "/v1/turns/missing/interrupt", `{"command_id":"x"}`, http.StatusNotFound)
	v1Call(t, server, http.MethodPost, "/v1/turns/1/interrupt", `{}`, http.StatusBadRequest)

	channel := &fakeConsole{exchanges: []consoleapi.Exchange{{ID: "c", Conversation: "console:opaque-channel", State: "queued", Input: "original"}}}
	server.SetConsole(channel)
	server.SetChannelHistory(readableChannels{})
	for _, path := range []string{"/v1/turns/c/steer", "/v1/turns/c/interrupt"} {
		v1Call(t, server, http.MethodPost, path, `{"command_id":"x"}`, http.StatusForbidden)
	}
	v1Call(t, server, http.MethodPost, "/v1/turns", `{"thread":"opaque-channel","input":"x","command_id":"x"}`, http.StatusForbidden)
	if channel.exchanges[0].State != "queued" || len(channel.exchanges) != 1 {
		t.Fatalf("a channel thread was acted in: %+v", channel.exchanges)
	}
	history := v1Decode[clientapi.ThreadDetail](t, v1Call(t, server, http.MethodGet, "/v1/threads/native/topic", "", http.StatusOK))
	if history.Thread.ID != "native/topic" || !history.Thread.ReadOnly {
		t.Fatalf("channel thread = %+v", history.Thread)
	}
	threads := v1Decode[clientapi.ThreadList](t, v1Call(t, server, http.MethodGet, "/v1/threads", "", http.StatusOK))
	var listed []string
	for _, thread := range threads.Threads {
		listed = append(listed, fmt.Sprintf("%s/%s/%v", thread.ID, thread.Transport, thread.ReadOnly))
	}
	legacy := v1Decode[struct {
		Conversations []consoleapi.Conversation `json:"conversations"`
	}](t, v1Call(t, server, http.MethodGet, "/console/conversations", "", http.StatusOK))
	var want []string
	for _, c := range legacy.Conversations {
		want = append(want, fmt.Sprintf("%s/%s/%v", c.ID, c.Transport, c.ReadOnly || c.Transport != "console"))
	}
	if strings.Join(listed, ",") != strings.Join(want, ",") || len(listed) != 2 {
		t.Fatalf("v1 threads %v, console conversations %v", listed, want)
	}
}

// Tasks are the console's task history, the same page under v1 names.
func TestClientV1TasksMatchConsoleTaskHistory(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	in := task.ProjectTransfer{Project: "p", Tasks: map[string]*task.Task{}}
	for i := range 7 {
		id := fmt.Sprintf("t-%02d", i)
		in.Tasks[id] = &task.Task{ID: id, ProjectID: "p", Transport: "console", Channel: "console:x", State: task.StateDone, UpdatedAt: time.Unix(int64(i), 0), Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliveryDelivered}}
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return task.ImportProjectTx(tx, in) }); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	server := serve(t, readmodel.New(readmodel.Sources{Tasks: tasks}), ServerConfig{Token: testToken})
	server.SetAdmin(&admin.Service{Tasks: tasks})
	consolePage := v1Decode[readmodel.TaskPage](t, v1Call(t, server, http.MethodGet, "/console/tasks?scope=conversation&scope_id=console:x&limit=3", "", http.StatusOK))
	v1Page := v1Decode[clientapi.TaskPage](t, v1Call(t, server, http.MethodGet, "/v1/tasks?thread=console:x&limit=3", "", http.StatusOK))
	if v1Page.Total != consolePage.Total || v1Page.NextCursor != consolePage.NextCursor || len(v1Page.Tasks) != len(consolePage.Items) || v1Page.Total != 7 {
		t.Fatalf("v1 %+v, console %+v", v1Page, consolePage)
	}
	for i := range v1Page.Tasks {
		if v1Page.Tasks[i].ID != consolePage.Items[i].ID || v1Page.Tasks[i].Thread != "console:x" || v1Page.Tasks[i].State != "done" {
			t.Fatalf("task %d: v1 %+v, console %+v", i, v1Page.Tasks[i], consolePage.Items[i])
		}
	}
	next := v1Decode[clientapi.TaskPage](t, v1Call(t, server, http.MethodGet, "/v1/tasks?thread=console:x&limit=3&cursor="+url.QueryEscape(v1Page.NextCursor), "", http.StatusOK))
	if len(next.Tasks) != 3 || next.Tasks[0].ID == v1Page.Tasks[0].ID {
		t.Fatalf("second page = %+v", next)
	}
	v1Call(t, server, http.MethodGet, "/v1/tasks?thread=a&project=b", "", http.StatusBadRequest)
	v1Call(t, server, http.MethodGet, "/v1/tasks?limit=0", "", http.StatusBadRequest)
}

// The v1 stream is the console stream: the same ids, resumed after the
// same cursor, with a reset when the cursor cannot be continued.
func TestClientV1EventsResumeLikeTheConsoleStream(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	server := serve(t, model, ServerConfig{Token: testToken})
	model.Publish(readmodel.Event{Kind: "console.sent", Conversation: "console:a", ReplyID: "r1", ExchangeID: "e1", Text: "hi"})
	model.Publish(readmodel.Event{Kind: "console.queue", Conversation: "console:b"})
	model.Publish(readmodel.Event{Kind: "console.reply", Conversation: "console:a", ReplyID: "r2", ExchangeID: "e1", Text: "hello"})
	legacy := openEvents(t, server, "", "")
	var want []sent
	for range 3 {
		want = append(want, nextEvent(t, legacy))
	}
	v1 := openV1Events(t, server, "", "")
	var seen []v1Event
	for range 3 {
		seen = append(seen, nextV1Event(t, v1))
	}
	for i, ev := range seen {
		if ev.id != want[i].id || ev.data.Cursor != ev.id || ev.data.SourceKind != want[i].kind {
			t.Fatalf("event %d: v1 %+v, console %+v", i, ev, want[i])
		}
	}
	if seen[0].event != "item.completed" || seen[0].data.Item != "r1" || seen[0].data.Thread != "console:a" || seen[1].event != "turn.updated" {
		t.Fatalf("events = %+v", seen)
	}
	for _, resume := range []struct{ name, header, after string }{{"header", seen[0].id, ""}, {"parameter", "", seen[0].id}} {
		t.Run(resume.name, func(t *testing.T) {
			stream := openV1Events(t, server, resume.header, resume.after)
			if got := nextV1Event(t, stream); got.id != seen[1].id {
				t.Fatalf("resumed at %+v, want %s", got, seen[1].id)
			}
		})
	}
	t.Run("thread", func(t *testing.T) {
		stream := openV1Events(t, server, "", seen[0].id+"&thread=console:a")
		if got := nextV1Event(t, stream); got.id != seen[2].id {
			t.Fatalf("thread a resumed at %+v, want %s", got, seen[2].id)
		}
	})
	t.Run("reset", func(t *testing.T) {
		stream := openV1Events(t, server, "other-epoch.9", "")
		if got := nextV1Event(t, stream); got.event != "reset" {
			t.Fatalf("an unknown cursor resumed with %+v", got)
		}
		if got := nextV1Event(t, stream); got.id != seen[0].id {
			t.Fatalf("after the reset came %+v, want every recent event", got)
		}
	})
}

type v1Event struct {
	event, id string
	data      clientapi.Event
}

func openV1Events(t *testing.T, server *Server, header, after string) *v1Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	path := server.URL() + "/v1/events"
	if after != "" {
		path += "?after=" + after
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if header != "" {
		req.Header.Set("Last-Event-ID", header)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("/v1/events = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	return &v1Stream{scanner: newScanner(res)}
}

func nextV1Event(t *testing.T, stream *v1Stream) v1Event {
	t.Helper()
	var got v1Event
	for stream.scanner.Scan() {
		line := stream.scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			got.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			got.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			if got.event != "reset" {
				got.data = v1Decode[clientapi.Event](t, []byte(strings.TrimPrefix(line, "data: ")))
			}
		case line == "" && got.event != "":
			return got
		}
	}
	t.Fatalf("stream ended: %v", stream.scanner.Err())
	return got
}

// A question asked during a v1 turn is shown in the thread and answered
// through v1 with the console's decision rules: validated choices, an
// idempotent command_id, and a conflict for a second decision.
func TestClientV1QuestionAnswerACPIntegration(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v %s", err, out)
	}
	f := newInteractionE2E(t, bin, false)
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		return f.request(t, method, path, []byte(body), "application/json", "test-token", want)
	}
	started := v1Decode[clientapi.Turn](t, call("POST", "/v1/turns", `{"thread":"console:e2e","input":"askme","command_id":"ask-1","expected_project":"scratch"}`, 200))
	q := f.pending(t, started.ID)
	detail := v1Decode[clientapi.ThreadDetail](t, call("GET", "/v1/threads/console:e2e", "", 200))
	var shown *clientapi.Question
	for _, item := range detail.Items {
		if item.Kind == clientapi.ItemQuestion && item.Question != nil && item.Question.ID == q.ID {
			shown = item.Question
		}
	}
	if shown == nil || shown.State != "pending" || shown.Turn != started.ID || shown.Kind != q.Kind || len(shown.Options) != len(q.Options) {
		t.Fatalf("thread does not show the pending question %+v: %+v", q, detail.Items)
	}
	path := "/v1/questions/" + q.ID + "/answer"
	call("POST", path, `{"command_id":"bad","decision":"accept","choice":"forged"}`, 400)
	first := call("POST", path, `{"command_id":"answer-1","decision":"accept","choice":"Blue"}`, 200)
	if replay := call("POST", path, `{"command_id":"answer-1","decision":"accept","choice":"Blue"}`, 200); string(replay) != string(first) {
		t.Fatalf("a replayed answer changed its receipt: %s then %s", first, replay)
	}
	answered := v1Decode[clientapi.AnswerResult](t, first).Question
	if answered.ID != q.ID || answered.State != "answered" || answered.Answer == nil || answered.Answer.Choice != "Blue" {
		t.Fatalf("answered = %+v", answered)
	}
	if code := v1ErrorCode(t, call("POST", path, `{"command_id":"answer-2","decision":"accept","choice":"Blue"}`, 409)); code != clientapi.CodeConflict {
		t.Fatalf("second decision = %s", code)
	}
	call("POST", "/v1/questions/missing/answer", `{"command_id":"x","decision":"cancel"}`, 404)
	if r := f.reply(t, consoleapi.Exchange{ID: started.ID, Conversation: "console:e2e"}); !strings.Contains(r.Text, "[answer: accept:Blue]") {
		t.Fatalf("the agent did not get the decision: %+v", r)
	}

	// A running turn is stopped with the console's own stop control.
	running := v1Decode[clientapi.Turn](t, call("POST", "/v1/turns", `{"thread":"console:e2e","input":"askme","command_id":"ask-2"}`, 200))
	waiting := f.pending(t, running.ID)
	stopped := v1Decode[clientapi.InterruptResult](t, call("POST", "/v1/turns/"+running.ID+"/interrupt", `{"command_id":"stop-2"}`, 200))
	if stopped.Turn.ID != running.ID || stopped.Control == nil {
		t.Fatalf("interrupt = %+v", stopped)
	}
	_ = f.reply(t, consoleapi.Exchange{ID: running.ID, Conversation: "console:e2e"})
	for _, got := range f.service.Questions("") {
		if got.ID == waiting.ID && got.State != "cancelled" {
			t.Fatalf("question survived the interrupt: %+v", got)
		}
	}
}

type v1Stream struct{ scanner *bufio.Scanner }

func newScanner(res *http.Response) *bufio.Scanner { return bufio.NewScanner(res.Body) }

// A thread reads newest first, a page at a time, until the cursor reaches
// its start; a reply expands into the items it is made of.
func TestClientV1ThreadItemsPageBackwards(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	f := &fakeConsole{}
	for i := range 5 {
		f.replies = append(f.replies,
			consoleapi.Reply{ID: fmt.Sprint("s", i), ExchangeID: fmt.Sprint("e", i), Conversation: "console:main", Kind: "sent", Input: fmt.Sprint("q", i), At: at.Add(time.Duration(2*i) * time.Second)},
			consoleapi.Reply{ID: fmt.Sprint("r", i), ExchangeID: fmt.Sprint("e", i), Conversation: "console:main", Kind: "reply", Text: fmt.Sprint("a", i), At: at.Add(time.Duration(2*i+1) * time.Second),
				Process: &consoleapi.Process{Reasoning: "why", Tools: []consoleapi.ToolCall{{ID: "t", Name: "Read", Status: "completed"}}}})
	}
	f.replies = append(f.replies, consoleapi.Reply{ID: "x", Conversation: "console:main", Kind: "sent", Input: "/cancel", Silent: true, At: at.Add(time.Minute)})
	server := serve(t, readmodel.New(readmodel.Sources{}), ServerConfig{Token: testToken})
	server.SetConsole(&questionlessConsole{f})
	var ids []string
	cursor := ""
	for page := 0; ; page++ {
		path := "/v1/threads/console:main?limit=4"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		detail := v1Decode[clientapi.ThreadDetail](t, v1Call(t, server, http.MethodGet, path, "", http.StatusOK))
		var got []string
		for _, item := range detail.Items {
			got = append(got, item.ID)
		}
		ids = append(got, ids...)
		if detail.NextCursor == "" {
			break
		}
		if page > 3 {
			t.Fatal("paging never reached the start of the thread")
		}
		cursor = detail.NextCursor
	}
	want := []string{}
	for i := range 5 {
		want = append(want, fmt.Sprint("s", i), fmt.Sprint("r", i, "#reasoning"), fmt.Sprint("r", i, "#tool-0"), fmt.Sprint("r", i))
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("items %v, want %v", ids, want)
	}
	if code := v1ErrorCode(t, v1Call(t, server, http.MethodGet, "/v1/threads/console:main?cursor=gone", "", http.StatusConflict)); code != clientapi.CodeStaleCursor {
		t.Fatalf("stale cursor = %s", code)
	}
	v1Call(t, server, http.MethodGet, "/v1/threads/console:main?limit=0", "", http.StatusBadRequest)
	v1Call(t, server, http.MethodGet, "/v1/threads/console:unknown", "", http.StatusNotFound)
}

// questionlessConsole is a console with nothing waiting on the owner.
type questionlessConsole struct{ *fakeConsole }

func (questionlessConsole) Questions(string) []consoleapi.PendingQuestion { return nil }

// readableChannels is the channel directory with its one thread readable.
type readableChannels struct{ channelHistoryStub }

func (readableChannels) Read(_ context.Context, id, _ string, _ int) (consoleapi.ChannelConversationHistory, error) {
	if id != "native/topic" {
		return consoleapi.ChannelConversationHistory{}, consoleapi.ErrChannelConversationNotFound
	}
	return consoleapi.ChannelConversationHistory{Conversation: consoleapi.Conversation{ID: id, Transport: "feishu", ReadOnly: true}, Replies: []consoleapi.Reply{{ID: "c1", Conversation: id, Kind: "reply", Text: "from the channel"}}}, nil
}
