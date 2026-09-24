package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/readmodel"
)

type initializingConsole struct {
	fakeConsole
	conversation, project string
	failure               error
}

func (c *initializingConsole) InitializeConversation(_ context.Context, conversation, project string) error {
	c.conversation, c.project = conversation, project
	return c.failure
}

func TestConsoleInitializationIsGuardedAndDoesNotSendMessages(t *testing.T) {
	s, err := NewServer(readmodel.New(readmodel.Sources{}), ServerConfig{Addr: "127.0.0.1:0", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	put := func(token, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, s.URL()+"/console/conversations/console%3Afresh/initialize", strings.NewReader(body))
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
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	if got := put("", `{"project":"workspace"}`); got != http.StatusUnauthorized {
		t.Fatalf("unguarded initialization: %d", got)
	}
	if got := put("test-token", `{"project":"workspace"}`); got != http.StatusNotImplemented {
		t.Fatalf("missing console: %d", got)
	}
	c := &initializingConsole{}
	s.SetConsole(c)
	if got := put("test-token", `{"project":`); got != http.StatusBadRequest {
		t.Fatalf("invalid body: %d", got)
	}
	if got := put("test-token", `{"project":"workspace"}`); got != http.StatusOK {
		t.Fatalf("initialization: %d", got)
	}
	if c.conversation != "console:fresh" || c.project != "workspace" || len(c.replies) != 0 || len(c.exchanges) != 0 {
		t.Fatalf("initialization sent a message or lost scope: %+v", c)
	}
	c.failure = errors.New("project is unavailable")
	if got := put("test-token", `{"project":"missing"}`); got != http.StatusBadRequest {
		t.Fatalf("failed initialization reported success: %d", got)
	}
}
