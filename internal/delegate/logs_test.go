package delegate

import (
	"bytes"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

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

	// The delivery line is written after the deliverer returns, so the
	// mailbox seeing the result does not mean the line is in the buffer.
	waitForLogLines(t, out,
		`^delegate: codex -> builder task #`+first.TaskID+` under #`+parent.ID+` on node-a task=`+first.TaskID+` parent=`+parent.ID+` attempt=\S+ conversation=chat agent=builder node=node-a$`,
		`^delegate: task #`+first.TaskID+` done on node-a task=`+first.TaskID+` parent=`+parent.ID+` attempt=\S+ conversation=chat agent=builder node=node-a$`,
		`^delegate: delivered 1 child result\(s\) into chat for task #`+parent.ID+` parent=`+parent.ID+` conversation=chat$`,
	)

	// docs/operations.md sends the operator from the creation line to the
	// attempt, so it must name the attempt the ledger goes on to record.
	lines := stripTimes(out.String())
	created := attemptOnLine(t, lines, `delegate: codex -> builder task #`+first.TaskID+` under`)
	settled := attemptOnLine(t, lines, `delegate: task #`+first.TaskID+` done`)
	if created != settled {
		t.Errorf("creation line says attempt=%s, settled line says attempt=%s", created, settled)
	}
}

// attemptOnLine reads the attempt field off the one line starting with prefix.
func attemptOnLine(t *testing.T, lines, prefix string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + prefix + `.* attempt=(\S+)`).FindStringSubmatch(lines)
	if m == nil {
		t.Fatalf("no attempt field on a line starting %q in log:\n%s", prefix, lines)
	}
	return m[1]
}

// waitForLogLines fails once every pattern has had its chance to appear,
// reporting the whole log so a missing line is read in context.
func waitForLogLines(t *testing.T, out *lockedLog, patterns ...string) {
	t.Helper()
	want := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		want[i] = regexp.MustCompile(`(?m)` + p)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		lines := stripTimes(out.String())
		missing := want[:0:0]
		for _, re := range want {
			if !re.MatchString(lines) {
				missing = append(missing, re)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, re := range missing {
				t.Errorf("missing %q in log:\n%s", re, lines)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stripTimes drops the standard log date and time prefix from each line so
// the assertions read as the operations table does.
func stripTimes(s string) string {
	return regexp.MustCompile(`(?m)^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d `).ReplaceAllString(s, "")
}
