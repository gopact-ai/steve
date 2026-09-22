package cluster

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/logs"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureRuntimeLog(t *testing.T) *syncBuffer {
	t.Helper()
	output := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(logs.NewHandler(output)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return output
}

func waitFor(t *testing.T, limit time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A business stop that outlives the shutdown timeout is slow, not stuck: the
// runtime says so with enough evidence to find the slow component, keeps the
// replica, and builds the next generation once the old one has stopped.
func TestSlowBusinessStopIsReportedAndTheNextGenerationStillStarts(t *testing.T) {
	output := captureRuntimeLog(t)
	nodes := testNodes(t, 1)
	nodes[0].config.ShutdownTimeout = 60 * time.Millisecond
	nodes[0].config.ShutdownDeadline = 5 * time.Second
	nodes[0].config.DiagnosticsDir = filepath.Join(t.TempDir(), "diagnostics")
	build := nodes[0].config.Activate
	var slow sync.Once
	nodes[0].config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
		stop, err := build(ctx, activation)
		if err != nil {
			return nil, err
		}
		return func(stopCtx context.Context) error {
			slow.Do(func() { time.Sleep(400 * time.Millisecond) })
			return stop(stopCtx)
		}, nil
	}
	r := openNode(t, nodes[0])
	first := ready(t, r)
	if err := r.RestartGeneration(first.Generation); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, "the next generation", func() bool {
		status := r.Status()
		return status.Ready && status.Generation > first.Generation
	})
	if status := r.Status(); status.Closed {
		t.Fatalf("a slow stop closed the runtime: %+v", status)
	}
	logged := output.String()
	if !strings.Contains(logged, "cluster: business generation 1 stop is slow") || !strings.Contains(logged, "phase=stop") {
		t.Fatalf("the slow stop was not reported:\n%s", logged)
	}
	dumps, _ := filepath.Glob(filepath.Join(nodes[0].config.DiagnosticsDir, "*.txt"))
	if len(dumps) != 1 {
		t.Fatalf("expected one goroutine dump for the slow stop, found %v", dumps)
	}
	raw, err := os.ReadFile(dumps[0])
	if err != nil || !strings.Contains(string(raw), "goroutine ") {
		t.Fatalf("the dump does not hold goroutine stacks: %v", err)
	}
	if !strings.Contains(logged, dumps[0]) {
		t.Fatalf("the log does not point at the dump %s:\n%s", dumps[0], logged)
	}
	closeRuntime(t, nodes[0], r)
}

// closeRuntime closes a runtime whose shutdown timeout is shorter than
// consensus takes to stop, so Close itself may report the timeout.
func closeRuntime(t *testing.T, n *clusterNode, r *Runtime) {
	t.Helper()
	n.runtime.Store(nil)
	_ = r.Close()
	select {
	case <-r.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("runtime did not finish closing")
	}
}

// A stop that never finishes cannot hold the node forever: at the deadline
// the runtime ends with the timeout as its failure, so the process owner can
// restart it, instead of waiting on a generation that will not stop.
func TestStuckBusinessStopEndsTheRuntimeAtTheDeadline(t *testing.T) {
	captureRuntimeLog(t)
	nodes := testNodes(t, 1)
	nodes[0].config.ShutdownTimeout = 50 * time.Millisecond
	nodes[0].config.ShutdownDeadline = 300 * time.Millisecond
	nodes[0].config.DiagnosticsDir = filepath.Join(t.TempDir(), "diagnostics")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	nodes[0].config.Activate = func(context.Context, Activation) (Deactivate, error) {
		return func(context.Context) error { <-release; return nil }, nil
	}
	r := openNode(t, nodes[0])
	first := ready(t, r)
	if err := r.RestartGeneration(first.Generation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck stop kept the runtime from ending")
	}
	if err := r.Failure(); !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("the runtime ended without naming the stuck stop: %v", err)
	}
	nodes[0].runtime.Store(nil)
}

// Every change of the business generation leaves one line saying what
// happened, to which generation, why and how long it took; a follower that
// keeps observing the same reason does not repeat it every poll.
func TestGenerationLifecycleIsLoggedOncePerChange(t *testing.T) {
	output := captureRuntimeLog(t)
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	first := ready(t, r)
	if err := r.RequestRebuild(first.Generation, errors.New("store write failed")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, "the next generation", func() bool {
		status := r.Status()
		return status.Ready && status.Generation > first.Generation
	})
	time.Sleep(200 * time.Millisecond)
	logged := output.String()
	for _, want := range []string{
		"cluster: business generation 1 ready",
		"cluster: business generation 1 retiring",
		"cause=\"store write failed\"",
		"cluster: business generation 1 stopped",
		"cluster: business generation 2 ready",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("missing %q in:\n%s", want, logged)
		}
	}
	if count := strings.Count(logged, "cluster: business generation 1 retiring"); count != 1 {
		t.Fatalf("retirement of one generation logged %d times:\n%s", count, logged)
	}
}

// An application that failed to start still hands back a stop that reports
// the same failure. Joining it is part of trying again, not a reason to
// leave the cluster: the replica stays and the next attempt builds.
func TestFailedActivationWhoseStopReportsTheFailureIsRetried(t *testing.T) {
	captureRuntimeLog(t)
	nodes := testNodes(t, 1)
	failure := errors.New("recover landings: resource is held")
	build := nodes[0].config.Activate
	var attempts sync.Mutex
	tries := 0
	nodes[0].config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
		attempts.Lock()
		tries++
		first := tries == 1
		attempts.Unlock()
		if first {
			return func(context.Context) error { return failure }, failure
		}
		return build(ctx, activation)
	}
	r := openNode(t, nodes[0])
	active := ready(t, r)
	if status := r.Status(); status.Closed || !status.Ready {
		t.Fatalf("the replica did not recover from a failed start: %+v", status)
	}
	if err := active.Ledger.Document("after-retry").Save([]byte("value")); err != nil {
		t.Fatal(err)
	}
}

// A generation whose application reports an error while stopping has
// still stopped; the next generation starts.
func TestStopErrorDoesNotEndTheRuntime(t *testing.T) {
	captureRuntimeLog(t)
	nodes := testNodes(t, 1)
	build := nodes[0].config.Activate
	var once sync.Once
	nodes[0].config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
		stop, err := build(ctx, activation)
		if err != nil {
			return nil, err
		}
		return func(stopCtx context.Context) error {
			err := stop(stopCtx)
			once.Do(func() { err = errors.New("application exited with an error") })
			return err
		}, nil
	}
	r := openNode(t, nodes[0])
	first := ready(t, r)
	if err := r.RestartGeneration(first.Generation); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, "the next generation", func() bool {
		status := r.Status()
		return status.Closed || status.Ready && status.Generation > first.Generation
	})
	if r.Status().Closed {
		t.Fatalf("a stop error ended the runtime: %v", r.Failure())
	}
}
