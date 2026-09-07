//go:build unix

package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper is a real detached process and real local HTTP listener. Its
// endpoint deliberately appears late to exercise retries during startup.
func TestMain(m *testing.M) {
	if os.Getenv("STEVE_DESKTOP_TEST_CHILD") == "1" {
		if err := serveDesktopTestChild(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func serveDesktopTestChild() error {
	if len(os.Args) != 4 || os.Args[1] != "run" || os.Args[2] != "--config" {
		return fmt.Errorf("unexpected helper arguments")
	}
	installed, err := Bootstrap(Options{StateDir: filepath.Dir(os.Args[3])})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(installed.Paths.Root, "test-process-starts"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, os.Getpid())
	f.Close()
	if err != nil {
		return err
	}
	time.Sleep(350 * time.Millisecond)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	cleanup, err := PublishEndpoint(installed.Paths.Config, "http://"+listener.Addr().String())
	if err != nil {
		return err
	}
	defer cleanup()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+installed.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"hub_id": installed.NodeID})
	})}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); _ = server.Close() }()
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func TestLaunchRetryDuringPreparationDoesNotStartAnotherBackend(t *testing.T) {
	t.Setenv("STEVE_DESKTOP_TEST_CHILD", "1")
	installed, err := Bootstrap(Options{StateDir: filepath.Join(t.TempDir(), "Steve")})
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	firstContext, firstCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer firstCancel()
	if _, err := EnsureRunning(firstContext, installed, binary); err == nil {
		t.Fatal("first launch did not exercise the readiness timeout")
	}
	var pending endpoint
	if err := readJSON(installed.Paths.Process, &pending); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pending.PID, syscall.SIGTERM) })
	retryContext, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer retryCancel()
	got, err := EnsureRunning(retryContext, installed, binary)
	if err != nil {
		t.Fatal(err)
	}
	if got.Started || got.PID != pending.PID {
		t.Fatalf("retry created another backend: %+v", got)
	}
	starts, err := os.ReadFile(filepath.Join(installed.Paths.Root, "test-process-starts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(string(starts))) != 1 {
		t.Fatalf("multiple backend starts: %s", starts)
	}
	if err := syscall.Kill(pending.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pending.PID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pending.PID) {
		t.Fatal("isolated test backend did not stop")
	}
}

func TestConcurrentLaunchHonorsWaitingWindowDeadline(t *testing.T) {
	installed, err := Bootstrap(Options{StateDir: filepath.Join(t.TempDir(), "Steve")})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(filepath.Join(installed.Paths.Root, ".desktop-start.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := EnsureRunning(ctx, installed, "/must/not/start"); err == nil {
		t.Fatal("launch ignored a canceled waiter")
	}
}
