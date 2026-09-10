package fleetlab

import (
	"strings"
	"testing"
	"time"
)

func TestCommandDeadlineStopsAHungCommand(t *testing.T) {
	_, err := run(50*time.Millisecond, "sleep", "30")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("hung command = %v", err)
	}
}

func TestSSHHostDoesNotIncludeIPv6Brackets(t *testing.T) {
	for addr, want := range map[string]string{
		"node.example:7701":  "node.example",
		"127.0.0.1:7701":     "127.0.0.1",
		"[2001:db8::1]:7701": "2001:db8::1",
	} {
		if got := hostOf(addr); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", addr, got, want)
		}
	}
}
