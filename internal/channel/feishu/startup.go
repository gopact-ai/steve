package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

const (
	// startupAttemptTimeout bounds one identity verification.
	startupAttemptTimeout = 15 * time.Second
	startupRetryBase      = time.Second
	startupRetryMax       = 2 * time.Minute
)

// verify reads the bot identity, retrying failures until it succeeds or ctx
// ends. The official long-connection client retries its own connection,
// before and after it is first established; this covers only the step
// before it. Retrying runs on the caller's goroutine, so returning ends it.
func (c *Channel) verify(ctx context.Context) (Identity, error) {
	for failures := 1; ; failures++ {
		attempt, cancel := context.WithTimeout(ctx, startupAttemptTimeout)
		identity, err := c.identify(attempt)
		cancel()
		if err == nil {
			return identity, nil
		}
		if ctx.Err() != nil {
			return Identity{}, ctx.Err()
		}
		delay := c.delay(failures)
		slog.Warn(fmt.Sprintf("feishu: startup failed; retrying in %s: %v", delay.Round(time.Millisecond), err),
			"attempt", failures, "retry_in", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Identity{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// startupRetryDelay doubles from startupRetryBase up to startupRetryMax.
// The upper half is random, so Hubs that failed together do not retry in
// step.
func startupRetryDelay(failures int) time.Duration {
	d := startupRetryMax
	if shift := failures - 1; shift < 16 {
		d = min(startupRetryBase<<max(shift, 0), startupRetryMax)
	}
	return d/2 + rand.N(d/2+1)
}
