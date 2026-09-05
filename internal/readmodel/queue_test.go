package readmodel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestConsoleQueueRoutes(t *testing.T) {
	server, err := NewServer(New(Sources{}), ServerConfig{Addr: "127.0.0.1:0", Token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	request := func(method, path, body, token string, status int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, server.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != status {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, res.StatusCode, status, raw)
		}
		return raw
	}
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/console/queue", `{"input":"hello"}`},
		{"GET", "/console/queue", ""},
		{"DELETE", "/console/queue/1", ""},
		{"PATCH", "/console/queue/1", `{"input":"edited"}`},
		{"POST", "/console/queue/1/steer", ""},
	} {
		request(route.method, route.path, route.body, "", http.StatusUnauthorized)
		request(route.method, route.path, route.body, "token", http.StatusNotImplemented)
	}
	server.SetConsole(&fakeConsole{})
	decode := func(raw []byte) Exchange {
		t.Helper()
		var e Exchange
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	request("POST", "/console/queue", `{`, "token", http.StatusBadRequest)
	request("POST", "/console/queue", `{"input":" "}`, "token", http.StatusBadRequest)
	first := decode(request("POST", "/console/queue", `{"input":"first","quotes":[{"conversation":"console:main","reply_id":"r1"}]}`, "token", http.StatusOK))
	if first.ID == "" || first.Conversation != "console:main" || len(first.Quotes) != 1 || first.Quotes[0].ReplyID != "r1" {
		t.Fatalf("enqueue = %+v", first)
	}
	second := decode(request("POST", "/console/queue", `{"input":"second","conversation":"console:other"}`, "token", http.StatusOK))
	if second.ID == first.ID {
		t.Fatal("reused exchange ID")
	}
	var listing struct {
		Queue []Exchange `json:"queue"`
	}
	if err := json.Unmarshal(request("GET", "/console/queue?conversation=console:main", "", "token", http.StatusOK), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Queue) != 1 || listing.Queue[0].ID != first.ID {
		t.Fatalf("queue = %+v", listing)
	}
	request("PATCH", "/console/queue/"+first.ID, `{"input":" "}`, "token", http.StatusBadRequest)
	edited := decode(request("PATCH", "/console/queue/"+first.ID, `{"input":"edited"}`, "token", http.StatusOK))
	if edited.Input != "edited" || edited.ID != first.ID {
		t.Fatalf("edit = %+v", edited)
	}
	steered := decode(request("POST", "/console/queue/"+first.ID+"/steer", "", "token", http.StatusOK))
	if steered.State != "running" || steered.Input != "!edited" || steered.ID != first.ID {
		t.Fatalf("steer = %+v", steered)
	}
	request("DELETE", "/console/queue/"+first.ID, "", "token", http.StatusConflict)
	request("PATCH", "/console/queue/"+first.ID, `{"input":"late"}`, "token", http.StatusConflict)
	request("POST", "/console/queue/"+first.ID+"/steer", "", "token", http.StatusConflict)
	request("DELETE", "/console/queue/"+second.ID, "", "token", http.StatusOK)
	request("DELETE", "/console/queue/"+second.ID, "", "token", http.StatusNotFound)
	request("PATCH", "/console/queue/missing", `{"input":"late"}`, "token", http.StatusNotFound)
	request("POST", "/console/queue/missing/steer", "", "token", http.StatusNotFound)
	raw := request("GET", "/console/queue?conversation=console:other", "", "token", http.StatusOK)
	if strings.TrimSpace(string(raw)) != `{"queue":[]}` {
		t.Fatalf("empty queue = %s", raw)
	}
}
