package feishu

import (
	"testing"
	"time"
)

// Retry waits double from the base up to the cap, never exceed it, and keep
// at least half of their step so jitter cannot collapse them into a spin.
func TestStartupRetryDelayIsBoundedExponentialWithJitter(t *testing.T) {
	for failures, step := range map[int]time.Duration{
		1: startupRetryBase, 2: 2 * startupRetryBase, 3: 4 * startupRetryBase,
		8: startupRetryMax, 64: startupRetryMax, 1 << 20: startupRetryMax,
	} {
		for range 200 {
			if d := startupRetryDelay(failures); d < step/2 || d > step {
				t.Fatalf("delay after %d failures = %s; want within [%s, %s]", failures, d, step/2, step)
			}
		}
	}
}
