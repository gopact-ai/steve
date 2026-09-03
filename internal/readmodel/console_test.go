package readmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type fakeConsole struct{ replies []Reply }

func (f *fakeConsole) Send(_ context.Context, conversation, input string) (Reply, error) {
	r := Reply{Conversation: conversation, Text: "did " + input, Kind: "reply"}
	f.replies = append(f.replies, r)
	return r, nil
}
func (f *fakeConsole) Replies(string) []Reply  { return f.replies }
func (f *fakeConsole) Conversations() []string { return []string{"console:main"} }
func (f *fakeConsole) Summaries(context.Context) []Conversation {
	return []Conversation{{ID: "console:main", Title: "main"}}
}
func (f *fakeConsole) Context(context.Context, string) (Context, error) {
	return Context{Conversation: "console:main"}, nil
}
func (f *fakeConsole) Verbs() []Verb { return []Verb{{Command: "/plan", Summary: "split"}} }
func (f *fakeConsole) SendCommand(ctx context.Context, conversation, input, _ string) (Reply, error) {
	return f.Send(ctx, conversation, input)
}
func (f *fakeConsole) Suggest(context.Context, string, string) []Suggestion { return nil }

// The console endpoints sit behind the same token as the snapshot and are
// off — honestly off — until a console is wired.
func TestConsoleEndpointsAreGuardedAndOptional(t *testing.T) {
	model := New(Sources{})
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
	if code != http.StatusOK || !strings.Contains(body, "did /fleet") {
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
	model := New(Sources{})
	server, err := NewServer(model, ServerConfig{Addr: "127.0.0.1:0", Token: "t0k"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	if res, _ := http.Get(server.URL() + "/"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("shell without token = %d", res.StatusCode)
	}
	shell, _ := http.Get(server.URL() + "/?token=t0k")
	body := make([]byte, 4096)
	n, _ := shell.Body.Read(body)
	shell.Body.Close()
	start := strings.Index(string(body[:n]), "assets/index-")
	if start < 0 {
		t.Fatalf("shell names no bundle: %s", body[:n])
	}
	end := start + strings.Index(string(body[start:n]), ".js") + 3
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
