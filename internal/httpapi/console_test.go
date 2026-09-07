package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

type fakeConsole struct {
	replies   []consoleapi.Reply
	exchanges []consoleapi.Exchange
}

func (f *fakeConsole) Send(_ context.Context, conversation, input string) (consoleapi.Reply, error) {
	r := consoleapi.Reply{ID: "reply-id", ExchangeID: "exchange-id", Conversation: conversation, Text: "did " + input, Kind: "reply"}
	f.replies = append(f.replies, r)
	return r, nil
}
func (f *fakeConsole) Replies(string) []consoleapi.Reply { return f.replies }
func (f *fakeConsole) Conversations() []string           { return []string{"console:main"} }
func (f *fakeConsole) Summaries(context.Context) []consoleapi.Conversation {
	return []consoleapi.Conversation{{ID: "console:main", Title: "main"}}
}
func (f *fakeConsole) Context(context.Context, string) (consoleapi.Context, error) {
	return consoleapi.Context{Conversation: "console:main"}, nil
}
func (f *fakeConsole) Verbs() []consoleapi.Verb {
	return []consoleapi.Verb{{Command: "/plan", Summary: "split"}}
}
func (f *fakeConsole) SendCommand(ctx context.Context, conversation, input, _ string) (consoleapi.Reply, error) {
	return f.Send(ctx, conversation, input)
}

func (f *fakeConsole) SendCommandWith(ctx context.Context, conversation, input, _ string, _ []consoleapi.QuoteRef) (consoleapi.Reply, error) {
	return f.Send(ctx, conversation, input)
}
func (f *fakeConsole) Enqueue(_ context.Context, conversation, input string, quotes []consoleapi.QuoteRef) (consoleapi.Exchange, error) {
	e := consoleapi.Exchange{ID: fmt.Sprint(len(f.exchanges) + 1), Conversation: conversation, Input: input, Quotes: quotes, State: "queued"}
	f.exchanges = append(f.exchanges, e)
	return e, nil
}
func (f *fakeConsole) EnqueueCommand(ctx context.Context, conversation, input, commandID string, quotes []consoleapi.QuoteRef) (consoleapi.Exchange, error) {
	for _, e := range f.exchanges {
		if commandID != "" && e.Conversation == conversation && e.Key == commandID {
			if e.Input != input {
				return consoleapi.Exchange{}, consoleapi.ErrCommandConflict
			}
			return e, nil
		}
	}
	e, err := f.Enqueue(ctx, conversation, input, quotes)
	if err == nil {
		e.Key = commandID
		f.exchanges[len(f.exchanges)-1] = e
	}
	return e, err
}
func (f *fakeConsole) Queue(conversation string) []consoleapi.Exchange {
	var list []consoleapi.Exchange
	for _, e := range f.exchanges {
		if e.Conversation == conversation {
			list = append(list, e)
		}
	}
	return list
}
func (f *fakeConsole) queued(id string) (int, error) {
	for i, e := range f.exchanges {
		if e.ID == id {
			if e.State != "queued" {
				return 0, consoleapi.ErrExchangeNotQueued
			}
			return i, nil
		}
	}
	return 0, consoleapi.ErrExchangeNotFound
}
func (f *fakeConsole) DeleteQueued(id string) error {
	i, err := f.queued(id)
	if err == nil {
		f.exchanges = append(f.exchanges[:i], f.exchanges[i+1:]...)
	}
	return err
}
func (f *fakeConsole) EditQueued(id, input string) (consoleapi.Exchange, error) {
	i, err := f.queued(id)
	if err != nil {
		return consoleapi.Exchange{}, err
	}
	f.exchanges[i].Input = input
	return f.exchanges[i], nil
}
func (f *fakeConsole) Steer(_ context.Context, id string) (consoleapi.Exchange, error) {
	i, err := f.queued(id)
	if err != nil {
		return consoleapi.Exchange{}, err
	}
	f.exchanges[i].Input = "!" + f.exchanges[i].Input
	f.exchanges[i].State = "running"
	return f.exchanges[i], nil
}
func (f *fakeConsole) Suggest(context.Context, string, string) []consoleapi.Suggestion { return nil }

// The console endpoints sit behind the same token as the snapshot and are
// off — honestly off — until a console is wired.
func TestConsoleEndpointsAreGuardedAndOptional(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	server, err := NewServer(model, ServerConfig{Addr: "127.0.0.1:0", Token: "t0k"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	post := func(token, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, server.URL()+"/console/send", bytes.NewReader([]byte(body)))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out bytes.Buffer
		_, _ = out.ReadFrom(res.Body)
		return res.StatusCode, out.String()
	}
	if code, _ := post("", `{"input":"/fleet"}`); code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", code)
	}
	if code, _ := post("t0k", `{"input":"/fleet"}`); code != http.StatusNotImplemented {
		t.Fatalf("no console wired = %d", code)
	}
	res, _ := http.Get(server.URL() + "/console/replies?token=t0k")
	var listing struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(res.Body).Decode(&listing)
	res.Body.Close()
	if listing.Enabled {
		t.Fatal("replies claimed the console was enabled")
	}
	server.SetConsole(&fakeConsole{})
	code, body := post("t0k", `{"conversation":"console:main","input":"/fleet"}`)
	if code != http.StatusOK || (!strings.Contains(body, "did /fleet") || !strings.Contains(body, `"exchange_id":"exchange-id"`)) {
		t.Fatalf("send = %d %s", code, body)
	}
	if code, _ := post("t0k", `{"input":"   "}`); code != http.StatusBadRequest {
		t.Fatalf("empty input = %d", code)
	}
	res, _ = http.Get(server.URL() + "/console/replies?token=t0k")
	var out bytes.Buffer
	_, _ = out.ReadFrom(res.Body)
	res.Body.Close()
	if !strings.Contains(out.String(), `"enabled":true`) || !strings.Contains(out.String(), "did /fleet") {
		t.Fatalf("replies = %s", out.String())
	}
}

// The shell needs the token; the bundle it names does not, or a browser
// that opened the page with ?token= would load an empty page.
func TestBundleIsServedOpenAndShellIsGuarded(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	server, err := NewServer(model, ServerConfig{Addr: "127.0.0.1:0", Token: "t0k"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	if res, _ := http.Get(server.URL() + "/"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("shell without token = %d", res.StatusCode)
	}
	shell, err := http.Get(server.URL() + "/?token=t0k")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(shell.Body)
	shell.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(body), "assets/index-")
	if start < 0 {
		t.Fatalf("shell names no bundle: %s", body)
	}
	end := start + strings.Index(string(body[start:]), ".js") + 3
	asset := string(body[start:end])
	res, err := http.Get(server.URL() + "/" + asset)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bundle %s without token = %d; the page would be blank", asset, res.StatusCode)
	}
	if res, _ := http.Get(server.URL() + "/state"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("state without token = %d", res.StatusCode)
	}
}

func (f *fakeConsole) Update(context.Context, string, consoleapi.ConversationPatch) error { return nil }
