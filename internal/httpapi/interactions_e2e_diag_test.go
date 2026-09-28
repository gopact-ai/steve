package httpapi

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// diagnoseLimit bounds the goroutine section of a failed wait so a race
// build under load still prints the queue state above it.
const diagnoseLimit = 48 << 10

// diagnose is what a failed wait in the ACP integration test prints: how
// long it waited, the console's own view of the conversation and its
// questions, and where the product's goroutines were at that moment. A turn
// that is still preparing (a slow before-snapshot under CPU contention) and
// a turn that is stuck read differently here, where "did not complete"
// alone reads the same.
func (f *interactionE2E) diagnose(conversation string, waited time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n--- waited %s (%s)\n", waited.Round(time.Millisecond), time.Now().UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "--- queue %s\n", conversation)
	for _, e := range f.service.Queue(conversation) {
		fmt.Fprintf(&b, "  %s state=%s input=%q enqueued=%s started=%s reply=%s\n", e.ID, e.State, e.Input, e.EnqueuedAt.Format(time.RFC3339Nano), e.StartedAt.Format(time.RFC3339Nano), e.ReplyID)
	}
	fmt.Fprintf(&b, "--- replies %s\n", conversation)
	for _, r := range f.service.Replies(conversation) {
		fmt.Fprintf(&b, "  %s exchange=%s kind=%s attempt=%s at=%s error=%q text=%q\n", r.ID, r.ExchangeID, r.Kind, r.AttemptID, r.At.Format(time.RFC3339Nano), r.Error, clip(r.Text, 200))
	}
	b.WriteString("--- questions\n")
	for _, q := range f.service.Questions("") {
		fmt.Fprintf(&b, "  %s exchange=%s kind=%s state=%s attempt=%s\n", q.ID, q.ExchangeID, q.Kind, q.State, q.AttemptID)
	}
	b.WriteString("--- goroutines in steve packages\n")
	b.WriteString(steveGoroutines(diagnoseLimit))
	return b.String()
}

// steveGoroutines keeps only the stacks that pass through this module, so
// the dump names the stage a turn is in without the runtime's idle pool.
func steveGoroutines(limit int) string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var b strings.Builder
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "gopact-ai/steve/internal/") {
			continue
		}
		if b.Len()+len(g) > limit {
			b.WriteString("  ... truncated\n")
			break
		}
		b.WriteString(g)
		b.WriteString("\n\n")
	}
	return b.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
