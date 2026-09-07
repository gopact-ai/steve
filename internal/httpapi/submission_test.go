package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
)

type submissionHandler struct{ calls atomic.Int32 }

func (h *submissionHandler) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	h.calls.Add(1)
	return turn.Result{Text: "handled " + req.Input}, nil
}

func TestHTTPSubmissionKeyIsSharedByQueueSendAndRestart(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	handler := &submissionHandler{}
	service := console.New(handler, "owner", nil)
	if err := service.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(readmodel.New(readmodel.Sources{}), ServerConfig{Addr: "127.0.0.1:0", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	server.SetConsole(service)
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { server.Close() })
	client := &http.Client{Timeout: 3 * time.Second}
	post := func(path, body string, wantStatus int) []byte {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != wantStatus {
			t.Fatalf("POST %s status=%d want=%d body=%s err=%v", path, resp.StatusCode, wantStatus, raw, err)
		}
		return raw
	}
	var first, replay consoleapi.Exchange
	if err := json.Unmarshal(post("/console/queue", `{"input":"hello","command_id":"key"}`, http.StatusOK), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(post("/console/queue", `{"input":" hello ","command_id":"key"}`, http.StatusOK), &replay); err != nil || replay.ID != first.ID {
		t.Fatalf("HTTP queue duplicated submission: %+v, %v", replay, err)
	}
	var answer struct{ Reply consoleapi.Reply }
	if err := json.Unmarshal(post("/console/send", `{"input":"hello","command_id":"key"}`, http.StatusOK), &answer); err != nil || answer.Reply.ExchangeID != first.ID {
		t.Fatalf("sync send did not join queue exchange: %+v, %v", answer, err)
	}
	for _, route := range []string{"/console/queue", "/console/send"} {
		raw := post(route, `{"input":"changed","command_id":"key"}`, http.StatusConflict)
		if !strings.Contains(string(raw), consoleapi.ErrCommandConflict.Error()) {
			t.Fatalf("conflict is not identifiable: %s", raw)
		}
	}
	post("/console/send", `{"conversation":"other","input":"hello","command_id":"key"}`, http.StatusOK)
	if handler.calls.Load() != 2 {
		t.Fatalf("conversation+key identity violated: calls=%d", handler.calls.Load())
	}
	restoredHandler := &submissionHandler{}
	restored := console.New(restoredHandler, "owner", nil)
	if err := restored.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	server.SetConsole(restored)
	var after struct{ Reply consoleapi.Reply }
	if err := json.Unmarshal(post("/console/send", `{"input":"hello","command_id":"key"}`, http.StatusOK), &after); err != nil || after.Reply.ID != answer.Reply.ID || restoredHandler.calls.Load() != 0 {
		t.Fatalf("HTTP restart retry executed again: %+v, %v, calls=%d", after, err, restoredHandler.calls.Load())
	}
}
