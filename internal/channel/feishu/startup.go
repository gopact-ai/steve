package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

const (
	// startupAttemptTimeout bounds one identity verification.
	startupAttemptTimeout = 15 * time.Second
	startupRetryBase      = time.Second
	startupRetryMax       = 2 * time.Minute
	// codeRateLimited is Feishu's request frequency limit.
	codeRateLimited = 99991400
)

// statusError is an HTTP status Feishu answered instead of a result.
type statusError int

func (e statusError) Error() string { return fmt.Sprintf("http %d", int(e)) }

// permanentError marks a failure Feishu repeats until the application or
// its configuration changes.
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// retryable reports whether a failed startup may succeed by waiting.
// Rejected credentials, missing configuration and a refused application
// are not; network failures, timeouts, 5xx and rate limits are. A failure
// that cannot be told apart, such as an undecodable proxy page, is retried:
// the wait is bounded and the last error stays visible.
func retryable(err error) bool {
	var permanent permanentError
	var illegal *larkcore.IllegalParamError
	var status statusError
	var code larkcore.CodeError
	var codeRef *larkcore.CodeError
	switch {
	case errors.As(err, &permanent), errors.As(err, &illegal):
		return false
	case errors.As(err, &status):
		return status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout
	case errors.As(err, &code):
		return code.Code == codeRateLimited
	case errors.As(err, &codeRef):
		return codeRef.Code == codeRateLimited
	}
	return true
}

// verify reads the bot identity, retrying retryable failures until it
// succeeds or ctx ends. The official long-connection client retries its
// own connection, before and after it is first established; this covers
// only the step before it. Retrying runs on the caller's goroutine, so
// returning ends it.
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
		if !retryable(err) {
			return Identity{}, err
		}
		delay := c.delay(failures)
		slog.Warn(fmt.Sprintf("feishu: startup failed; retrying in %s: %v", delay.Round(time.Millisecond), err),
			"attempt", failures, "retry_in", delay)
		if c.onRetry != nil {
			c.onRetry(StartRetry{Failures: failures, Next: time.Now().Add(delay), Err: err})
		}
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
