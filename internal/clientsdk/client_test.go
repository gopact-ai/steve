package clientsdk_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/clientapi"
	"github.com/gopact-ai/steve/internal/clientsdk"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"github.com/gopact-ai/steve/internal/view"
)

const token = "owner-token"

// coordinator answers "echo …" at once, asks the owner on "ask", and holds
// "hold" until it is stopped.
type coordinator struct {
	turntest.IdleCoordinator
	started chan string
	mu      sync.Mutex
	held    map[string]context.CancelFunc
}

func (c *coordinator) InitializeConversation(context.Context, string, string, string) error {
	return nil
}

func (c *coordinator) Context(_ context.Context, conversation string) (turn.Context, error) {
	return turn.Context{Conversation: conversation, Project: &turn.ContextProject{ID: "scratch", Bound: true}}, nil
}

func (c *coordinator) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	switch {
	case req.Input == "/cancel":
		c.mu.Lock()
		if stop := c.held[req.Admission.StopTarget]; stop != nil {
			stop()
		}
		c.mu.Unlock()
		return turn.Result{Text: "stopped"}, nil
	case req.Input == "ask":
		answer, err := req.OnAskUser(ctx, view.Question{Message: "Pick a colour", Choices: []view.Choice{{Value: "blue", Label: "Blue"}, {Value: "red", Label: "Red"}}})
		if err != nil {
			return turn.Result{}, err
		}
		return turn.Result{Text: "picked " + answer.Value + answer.Decision}, nil
	case req.Input == "hold":
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		c.mu.Lock()
		c.held[req.Admission.ExchangeID] = cancel
		c.mu.Unlock()
		c.started <- req.Admission.ExchangeID
		<-ctx.Done()
		return turn.Result{Text: "held"}, nil
	}
	return turn.Result{Text: "handled " + req.Input}, nil
}

// backend serves the real protocol handler over a real console and read
// model; the model can be replaced to stand for a server restart.
type backend struct {
	model   atomic.Pointer[readmodel.Model]
	console consoleapi.Console
}

func (b *backend) Model() clientapi.Model                    { return b.model.Load() }
func (b *backend) Console() consoleapi.Console               { return b.console }
func (b *backend) Attempts() clientapi.Attempts              { return nil }
func (b *backend) ChannelHistory() consoleapi.ChannelHistory { return nil }
func (b *backend) ServerVersion() string                     { return "test" }

type fixture struct {
	server  *httptest.Server
	client  *clientsdk.Client
	backend *backend
	coord   *coordinator
	// drop ends every open event stream, as a lost connection does.
	drop func()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	model := readmodel.New(readmodel.Sources{})
	coord := &coordinator{started: make(chan string, 4), held: map[string]context.CancelFunc{}}
	service := console.New(coord, "owner", model)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = service.Shutdown(ctx)
	})
	b := &backend{console: service}
	b.model.Store(model)
	mux := http.NewServeMux()
	var mu sync.Mutex
	var streams []context.CancelFunc
	clientapi.New(b).Routes(mux, func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/v1/events" {
				ctx, cancel := context.WithCancel(r.Context())
				mu.Lock()
				streams = append(streams, cancel)
				mu.Unlock()
				r = r.WithContext(ctx)
			}
			next(w, r)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	drop := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, cancel := range streams {
			cancel()
		}
		streams = nil
	}
	return &fixture{server: server, client: clientsdk.New(server.URL, token, nil), backend: b, coord: coord, drop: drop}
}

func ctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitThread re-reads a thread until done holds.
func waitThread(t *testing.T, c *clientsdk.Client, id string, done func(clientapi.ThreadDetail) bool) clientapi.ThreadDetail {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		detail, err := c.Thread(ctx(t), id, clientsdk.Page{})
		if err != nil {
			t.Fatal(err)
		}
		if done(detail) {
			return detail
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread %s never reached the expected state: %+v", id, detail)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assistantSaid(text string) func(clientapi.ThreadDetail) bool {
	return func(d clientapi.ThreadDetail) bool {
		for _, item := range d.Items {
			if item.Kind == clientapi.ItemMessage && item.Role == "assistant" && item.Text == text {
				return true
			}
		}
		return false
	}
}

func TestInitializeRefusesAWrongTokenAndAnOlderServer(t *testing.T) {
	f := newFixture(t)
	init, err := f.client.Initialize(ctx(t))
	if err != nil || init.Protocol != clientapi.Protocol || !strings.Contains(strings.Join(init.Capabilities, ","), "turns") {
		t.Fatalf("initialize = %+v, %v", init, err)
	}
	_, err = clientsdk.New(f.server.URL, "wrong", nil).Initialize(ctx(t))
	var refused *clientsdk.Error
	if !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized {
		t.Fatalf("wrong token = %v", err)
	}
	// A build before the protocol serves its web page for every path.
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	defer page.Close()
	if _, err := clientsdk.New(page.URL, token, nil).Initialize(ctx(t)); !errors.Is(err, clientsdk.ErrNotV1) {
		t.Fatalf("an older server = %v, want ErrNotV1", err)
	}
}

// A thread is created idempotently, a turn's command_id replays the same
// turn, and the reply is read back as items.
func TestThreadAndTurnRoundTrip(t *testing.T) {
	f := newFixture(t)
	created, err := f.client.CreateThread(ctx(t), clientapi.CreateThread{ID: "sdk", Project: "scratch"})
	if err != nil || created.Thread.ID != "console:sdk" {
		t.Fatalf("create = %+v, %v", created.Thread, err)
	}
	if again, err := f.client.CreateThread(ctx(t), clientapi.CreateThread{ID: "sdk", Project: "scratch"}); err != nil || again.Thread.ID != created.Thread.ID {
		t.Fatalf("create again = %+v, %v", again.Thread, err)
	}
	req := clientapi.StartTurn{Thread: "console:sdk", Input: "echo one", CommandID: "c1", ExpectedProject: "scratch"}
	first, err := f.client.StartTurn(ctx(t), req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.client.StartTurn(ctx(t), req)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay = %+v, %v; want turn %s", replay, err, first.ID)
	}
	req.Input = "something else"
	if _, err := f.client.StartTurn(ctx(t), req); !clientsdk.IsCode(err, clientapi.CodeCommandConflict) {
		t.Fatalf("reused command_id = %v", err)
	}
	detail := waitThread(t, f.client, "console:sdk", assistantSaid("handled echo one"))
	users := 0
	for _, item := range detail.Items {
		if item.Kind == clientapi.ItemMessage && item.Role == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("one command made %d user messages: %+v", users, detail.Items)
	}
	threads, err := f.client.Threads(ctx(t))
	if err != nil || len(threads) != 1 || threads[0].ID != "console:sdk" {
		t.Fatalf("threads = %+v, %v", threads, err)
	}
	if _, err := f.client.Thread(ctx(t), "console:missing", clientsdk.Page{}); !clientsdk.IsCode(err, clientapi.CodeNotFound) {
		t.Fatalf("missing thread = %v", err)
	}
}

func TestQuestionAnswerAndInterrupt(t *testing.T) {
	f := newFixture(t)
	if _, err := f.client.CreateThread(ctx(t), clientapi.CreateThread{ID: "q", Project: "scratch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.StartTurn(ctx(t), clientapi.StartTurn{Thread: "console:q", Input: "ask", CommandID: "a1"}); err != nil {
		t.Fatal(err)
	}
	var question *clientapi.Question
	waitThread(t, f.client, "console:q", func(d clientapi.ThreadDetail) bool {
		for _, item := range d.Items {
			if item.Question != nil && item.Question.State == "pending" {
				question = item.Question
				return true
			}
		}
		return false
	})
	answer := clientapi.QuestionAnswer{CommandID: "ans", Decision: "accept", Choice: "red"}
	got, err := f.client.Answer(ctx(t), question.ID, answer)
	if err != nil || got.Question.State != "answered" {
		t.Fatalf("answer = %+v, %v", got, err)
	}
	if again, err := f.client.Answer(ctx(t), question.ID, answer); err != nil || again.Question.State != "answered" {
		t.Fatalf("repeated answer = %+v, %v", again, err)
	}
	if _, err := f.client.Answer(ctx(t), question.ID, clientapi.QuestionAnswer{CommandID: "other", Decision: "decline"}); !clientsdk.IsCode(err, clientapi.CodeConflict) {
		t.Fatalf("second decision = %v", err)
	}
	waitThread(t, f.client, "console:q", assistantSaid("picked red"))

	held, err := f.client.StartTurn(ctx(t), clientapi.StartTurn{Thread: "console:q", Input: "hold", CommandID: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.coord.started
	stop, err := f.client.Interrupt(ctx(t), held.ID, "stop-1")
	if err != nil || stop.Control == nil {
		t.Fatalf("interrupt = %+v, %v", stop, err)
	}
	waitThread(t, f.client, "console:q", func(d clientapi.ThreadDetail) bool {
		for _, turn := range d.Turns {
			if turn.ID == held.ID {
				return turn.State != clientapi.TurnRunning && turn.State != clientapi.TurnQueued
			}
		}
		return true
	})
}

// Follow resumes after the last cursor when the connection drops, so no
// event is delivered twice, and reports a reset when the server can no
// longer continue from it, as after a restart.
func TestFollowResumesAndResets(t *testing.T) {
	f := newFixture(t)
	model := f.backend.model.Load()
	updates := f.client.Follow(ctx(t), "", "")
	expectLink(t, updates, clientsdk.Connected)
	model.Publish(readmodel.Event{Kind: "console.sent", Conversation: "console:a", ReplyID: "r1", ExchangeID: "e1", Text: "hi"})
	first := expectEvent(t, updates)
	if first.Type != clientapi.EventItemCompleted || first.Item != "r1" {
		t.Fatalf("first event = %+v", first)
	}
	f.drop()
	expectLink(t, updates, clientsdk.Disconnected)
	model.Publish(readmodel.Event{Kind: "console.reply", Conversation: "console:a", ReplyID: "r2", ExchangeID: "e1", Text: "hello"})
	expectLink(t, updates, clientsdk.Connected)
	second := expectEvent(t, updates)
	if second.Item != "r2" {
		t.Fatalf("after reconnecting got %+v, want only r2 (r1 again would be a duplicate)", second)
	}

	// A restart starts a new epoch: the old cursor cannot be continued.
	restarted := readmodel.New(readmodel.Sources{})
	restarted.Publish(readmodel.Event{Kind: "console.queue", Conversation: "console:a"})
	f.backend.model.Store(restarted)
	f.drop()
	expectLink(t, updates, clientsdk.Disconnected)
	expectLink(t, updates, clientsdk.Connected)
	reset := next(t, updates)
	if !reset.Reset {
		t.Fatalf("after a restart got %+v, want a reset", reset)
	}
	if ev := expectEvent(t, updates); ev.Type != clientapi.EventTurnUpdated {
		t.Fatalf("after the reset got %+v, want the restarted server's events", ev)
	}
}

func next(t *testing.T, updates <-chan clientsdk.Update) clientsdk.Update {
	t.Helper()
	select {
	case u, ok := <-updates:
		if !ok {
			t.Fatal("follow ended")
		}
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("no update")
	}
	return clientsdk.Update{}
}

func expectLink(t *testing.T, updates <-chan clientsdk.Update, want clientsdk.Link) {
	t.Helper()
	if u := next(t, updates); u.Link != want || u.Event != nil || u.Reset {
		t.Fatalf("update = %+v, want link %s", u, want)
	}
}

func expectEvent(t *testing.T, updates <-chan clientsdk.Update) clientapi.Event {
	t.Helper()
	u := next(t, updates)
	if u.Event == nil {
		t.Fatalf("update = %+v, want an event", u)
	}
	return *u.Event
}

// A refusal that is not a v1 error body, such as a proxy's error page, is
// kept as one bounded line, so it cannot fill a status line or memory.
func TestNonProtocolRefusalIsTruncatedToOneLine(t *testing.T) {
	page := strings.Repeat("<p>Bad gateway\r\n</p>\n", 1<<20)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(page))
	}))
	defer proxy.Close()
	_, err := clientsdk.New(proxy.URL, token, nil).Initialize(ctx(t))
	var refused *clientsdk.Error
	if !errors.As(err, &refused) || refused.Status != http.StatusBadGateway {
		t.Fatalf("err = %v", err)
	}
	if len(refused.Message) > 4<<10 || strings.ContainsAny(refused.Message, "\r\n") || !strings.HasPrefix(refused.Message, "<p>Bad gateway </p>") || !strings.HasSuffix(refused.Message, "…") {
		t.Fatalf("message is %d bytes: %.120q", len(refused.Message), refused.Message)
	}
	if !utf8.ValidString(refused.Message) {
		t.Fatal("the message was cut inside a character")
	}
}
