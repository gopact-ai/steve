package delegate

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/view"
)

func TestSilenceSourceDoesNotRetuneRunningChild(t *testing.T) {
	w := newWorld(t)
	w.running(t, "codex")
	var silence atomic.Int64
	silence.Store(int64(time.Minute))
	w.service.SilenceSource = func() time.Duration { return time.Duration(silence.Load()) }
	w.sessions.run = func(ctx context.Context, _ func(view.Progress)) (string, error) {
		silence.Store(int64(time.Nanosecond))
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(30 * time.Millisecond):
			return "done\nREF: git unchanged", nil
		}
	}
	result, err := w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "already running", Requires: []string{"gpu"}})
	if err != nil || result.State != "done" {
		t.Fatalf("policy interrupted existing child: %+v %v", result, err)
	}
	w.sessions.run = func(ctx context.Context, _ func(view.Progress)) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	result, err = w.service.Delegate(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "new child", Requires: []string{"gpu"}})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("new child did not use updated silence limit: %+v %v", result, err)
	}
}
