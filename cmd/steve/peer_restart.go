package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/processrestart"
)

// A peer whose runtime stops on its own — for example a business generation
// that would not stop — continues as a fresh copy of itself, keeping its
// process ID, arguments and environment, so nothing outside has to notice
// and restart it. Failures that keep repeating end the process instead.
const (
	failureRestartEnv    = "STEVE_FAILURE_RESTARTS"
	failureRestartLimit  = 3
	failureRestartWindow = 10 * time.Minute
)

// admitFailureRestart decides whether one more restart fits in the window
// and returns the record to carry into the restarted process.
func admitFailureRestart(record string, now time.Time) (string, bool) {
	var kept []string
	for _, field := range strings.Split(record, ",") {
		at, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil {
			continue
		}
		if when := time.Unix(at, 0); when.After(now) || now.Sub(when) >= failureRestartWindow {
			continue
		}
		kept = append(kept, strconv.FormatInt(at, 10))
	}
	if len(kept) >= failureRestartLimit {
		return strings.Join(kept, ","), false
	}
	return strings.Join(append(kept, strconv.FormatInt(now.Unix(), 10)), ","), true
}

// restartAfterFailure replaces this process with a fresh copy when the
// failure budget allows; otherwise, or when that is impossible, it returns
// the failure for the process to exit with.
func restartAfterFailure(cause error) error {
	if !processrestart.Supported() {
		return cause
	}
	record, ok := admitFailureRestart(os.Getenv(failureRestartEnv), time.Now())
	if !ok {
		slog.Error(fmt.Sprintf("steve: peer stopped on its own %d times within %s; exiting: %v", failureRestartLimit, failureRestartWindow, cause))
		return cause
	}
	if err := os.Setenv(failureRestartEnv, record); err != nil {
		return cause
	}
	slog.Error(fmt.Sprintf("steve: peer stopped on its own; restarting: %v", cause), "restarts", len(strings.Split(record, ",")))
	if err := processrestart.ReexecCurrent(); err != nil {
		return fmt.Errorf("%w; restart failed: %v", cause, err)
	}
	return cause
}
