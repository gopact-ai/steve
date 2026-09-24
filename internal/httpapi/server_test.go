package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/gopact"
	"github.com/gopact-ai/steve/internal/readmodel"
)

// testToken is the owner token of servers the tests start with serve.
const testToken = "test-token"

func serve(t *testing.T, model *readmodel.Model, cfg ServerConfig) *Server {
	t.Helper()
	server, err := NewServer(model, cfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func TestServerServesStateEventsAndPage(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	server := serve(t, model, ServerConfig{Token: testToken})

	res, err := http.Get(server.URL() + "/state?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var snap readmodel.Snapshot
	if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Nodes) != 1 || snap.Hub.Node != "hub-1" {
		t.Fatalf("snapshot over http = %+v", snap.Hub)
	}

	page, err := http.Get(server.URL() + "/?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(string(body)), "steve") {
		t.Fatal("dashboard did not render")
	}

	// The stream must deliver without the client polling.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = model.Emit(context.Background(), gopact.Event{Type: "node.failed", NodeID: "build"})
	}()
	scanner := bufio.NewScanner(stream.Body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "node.failed") {
			return
		}
	}
	t.Fatal("the event never reached the stream")
}

// A server without a token is refused on any address: loopback keeps other
// machines out, not other local users or processes, and the console grants
// owner operations.
func TestServerRequiresAToken(t *testing.T) {
	model := readmodel.New(readmodel.Sources{Hub: readmodel.Hub{Node: "hub-1"}})
	for _, addr := range []string{"", "127.0.0.1:0", "0.0.0.0:0"} {
		for _, token := range []string{"", "  "} {
			if server, err := NewServer(model, ServerConfig{Addr: addr, Token: token}); err == nil {
				_ = server.Close()
				t.Fatalf("addr %q was accepted with token %q", addr, token)
			} else if !strings.Contains(err.Error(), "token") {
				t.Fatalf("addr %q: error = %v", addr, err)
			}
		}
	}
	server, err := NewServer(model, ServerConfig{Addr: "0.0.0.0:0", Token: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })

	url := strings.Replace(server.URL(), "0.0.0.0", "127.0.0.1", 1)
	url = strings.Replace(url, "[::]", "127.0.0.1", 1)
	res, err := http.Get(url + "/state")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", res.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, url+"/state", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	ok, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("with token = %d, want 200", ok.StatusCode)
	}
}
