// Command steve runs Steve: the chief of staff you talk to, the tasks it
// runs for you and the approvals it brings back.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/localtoken"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/sameorigin"
	"github.com/gopact-ai/steve/internal/server"
)

// ReleaseVersion is set by the release build.
var ReleaseVersion string

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:])
	case "version", "--version":
		fmt.Printf("steve %s %s/%s\n", version(), runtime.GOOS, runtime.GOARCH)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "steve:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: steve run --config <config.json> | steve version")
}

func version() string {
	build := nodewire.Version()
	if ReleaseVersion == "" {
		return build
	}
	return ReleaseVersion + " (" + build + ")"
}

func run(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	path := flags.String("config", "config.json", "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv, err := server.New(ctx, cfg, server.Options{})
	if err != nil {
		return err
	}
	defer srv.Close()
	addr := config.ConsoleAddr(cfg.Gateway.ReadModelAddr)
	if srv.Token == "" {
		// Off loopback a token must be configured; on loopback one is
		// generated and kept beside the state.
		if !sameorigin.LoopbackListener(addr) {
			return errors.New("gateway.read_model_token is required when listening beyond loopback")
		}
		if srv.Token, err = localtoken.Resolve(filepath.Dir(cfg.Gateway.StatePath)); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	slog.Info("steve: serving", "addr", listener.Addr().String(), "version", version())
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err := <-done:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdown)
}
