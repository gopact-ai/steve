// gate owns an isolated fleet, then runs the existing HTTP acceptance client.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/e2e/fleetlab"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "LAB FAIL:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, out io.Writer) (err error) {
	flags := flag.NewFlagSet("lab-gate", flag.ContinueOnError)
	flags.SetOutput(out)
	scenario := flags.String("scenario", "delegate", "delegate or autonomous")
	if err = flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || (*scenario != "delegate" && *scenario != "autonomous") {
		return errors.New("use -scenario delegate or autonomous, without positional arguments")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	started := time.Now()
	h, err := fleetlab.OpenHub(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, h.Close()) }()
	fmt.Fprintf(out, "LAB READY elapsed=%s dir=%s hub=%s\n", time.Since(started).Round(time.Millisecond), h.Dir, h.URL)
	if err = h.RunGate(ctx, *scenario, out); err != nil {
		fmt.Fprintln(out, h.Logs())
		return err
	}
	return nil
}
