package delegate

import (
	"bytes"
	"log/slog"
	"regexp"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/logs"
)

// lockedLog collects log lines written from any goroutine.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// The lines the troubleshooting table in docs/operations.md greps for keep
// their text; the identifiers follow as fields.
func TestDelegateLogLinesKeepTheirTextAndCarryFields(t *testing.T) {
	out := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(logs.NewHandler(out)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	w := newWorld(t)
	parent := w.running(t, "codex")
	box := &mailbox{}
	w.service.SetDeliverer(box.deliver)
	first, release := startBlocked(t, w, "codex")
	release()
	box.wait(t, 1)

	lines := stripTimes(out.String())
	for _, want := range []string{
		`^delegate: codex -> builder task #` + first.TaskID + ` under #` + parent.ID + ` on node-a task=` + first.TaskID + ` parent=` + parent.ID + ` attempt=\S+ conversation=chat agent=builder node=node-a$`,
		`^delegate: task #` + first.TaskID + ` done on node-a task=` + first.TaskID + ` parent=` + parent.ID + ` attempt=\S+ conversation=chat agent=builder node=node-a$`,
		`^delegate: delivered 1 child result\(s\) into chat for task #` + parent.ID + ` parent=` + parent.ID + ` conversation=chat$`,
	} {
		if !regexp.MustCompile(`(?m)` + want).MatchString(lines) {
			t.Errorf("missing %q in log:\n%s", want, lines)
		}
	}
}

// stripTimes drops the standard log date and time prefix from each line so
// the assertions read as the operations table does.
func stripTimes(s string) string {
	return regexp.MustCompile(`(?m)^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d `).ReplaceAllString(s, "")
}
