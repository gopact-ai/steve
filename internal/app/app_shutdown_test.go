package app

import (
	"strings"
	"testing"
	"time"
)

// A shutdown step that takes long names where it was registered, so a slow
// stop points at the component rather than at the application as a whole.
func TestSlowShutdownStepIsNamedByWhereItWasRegistered(t *testing.T) {
	output := captureLog(t)
	life := &applicationLifetime{slowStep: 20 * time.Millisecond}
	life.Defer(func() {})
	life.Defer(func() { time.Sleep(60 * time.Millisecond) })
	if err := life.Close(); err != nil {
		t.Fatal(err)
	}
	logged := output.String()
	if !strings.Contains(logged, "app: shutdown step is slow") || !strings.Contains(logged, "app_shutdown_test.go:") {
		t.Fatalf("the slow step was not named:\n%s", logged)
	}
	if strings.Count(logged, "shutdown step is slow") != 1 {
		t.Fatalf("a fast step was reported as slow:\n%s", logged)
	}
}
