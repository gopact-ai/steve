package app

import (
	"bytes"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// captureLog routes both slog and log.Printf output into a buffer for the
// duration of the test, restoring the process-wide defaults afterwards.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previousLogger := slog.Default()
	previousWriter, previousFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(newLogHandler(&output)))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return &output
}

func TestLogHandlerKeepsThePrintfLineShape(t *testing.T) {
	output := captureLog(t)
	slog.Info("steve: doctor passed")
	slog.With("node", "n 1").WithGroup("g").Warn("console: resume task #7: boom", "task", "7", "err", errors.New("boom"), "at", time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC), "empty", "")
	slog.Error("gateway: turn failed: chat=c error=x", slog.Group("card", "id", "c=1", "n", 2))
	log.Printf("turn: timing total=%s", "1s")
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	want := []string{
		"steve: doctor passed",
		`console: resume task #7: boom node="n 1" g.task=7 g.err=boom g.at=2026-09-08T10:00:00Z g.empty="" level=WARN`,
		`gateway: turn failed: chat=c error=x card.id="c=1" card.n=2 level=ERROR`,
		"turn: timing total=1s",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), output.String())
	}
	for i, line := range lines {
		stamp, rest, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("line %d has no prefix: %q", i, line)
		}
		if _, err := time.Parse("2006/01/02", stamp); err != nil {
			t.Fatalf("line %d date prefix %q: %v", i, stamp, err)
		}
		if _, rest, ok = strings.Cut(rest, " "); !ok || rest != want[i] {
			t.Fatalf("line %d:\n got %q\nwant %q", i, line, want[i])
		}
	}
}
