package main

import (
	"strconv"
	"testing"
	"time"
)

// A peer that stops on its own is started again, but a failure that
// repeats within the window ends the process instead of spinning.
func TestFailureRestartsAreBoundedWithinTheWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	record := ""
	for i := 0; i < failureRestartLimit; i++ {
		next, ok := admitFailureRestart(record, now.Add(time.Duration(i)*time.Minute))
		if !ok {
			t.Fatalf("restart %d was refused", i+1)
		}
		record = next
	}
	if _, ok := admitFailureRestart(record, now.Add(time.Duration(failureRestartLimit)*time.Minute)); ok {
		t.Fatal("a failure repeating within the window was restarted again")
	}
	later := now.Add(failureRestartWindow + time.Duration(failureRestartLimit)*time.Minute)
	if _, ok := admitFailureRestart(record, later); !ok {
		t.Fatal("failures older than the window still count")
	}
}

func TestFailureRestartRecordIgnoresUnreadableEntries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	next, ok := admitFailureRestart("garbage,"+strconv.FormatInt(now.Add(time.Hour).Unix(), 10), now)
	if !ok || next != strconv.FormatInt(now.Unix(), 10) {
		t.Fatalf("unreadable or future entries were counted: %q %v", next, ok)
	}
}
