package turn

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"testing"

	"github.com/gopact-ai/steve/internal/logs"
)

func TestTurnClockReportsOneStructuredLine(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(logs.NewHandler(&out)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	clock := newTurnClock()
	clock.mark("gate")
	clock.mark("assemble")
	clock.report(context.Background(), "att-1")

	line := regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d turn: timing attempt=att-1 gate=\d+ assemble=\d+ total=\d+\n$`)
	if !line.MatchString(out.String()) {
		t.Fatalf("timing line = %q", out.String())
	}
	if attrs := clock.attrs("att-1"); len(attrs) != 4 || attrs[0].Key != "attempt" || attrs[3].Key != "total" {
		t.Fatalf("attrs = %v", attrs)
	}
}

func TestNilTurnClockRecordsNothing(t *testing.T) {
	var clock *turnClock
	clock.mark("gate")
	if attrs := clock.attrs("att-2"); len(attrs) != 1 || attrs[0].Key != "attempt" {
		t.Fatalf("attrs = %v", attrs)
	}
}
