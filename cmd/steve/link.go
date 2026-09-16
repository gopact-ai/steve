package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

// linkCmd is the far end of an SSH link: the hub runs it on this machine
// over the session it opened and multiplexes the cluster protocol through
// its stdin and stdout. It ends with the session.
func linkCmd(args []string) error {
	flags := flag.NewFlagSet("link", flag.ContinueOnError)
	var listens, allowed repeatedFlag
	flags.Var(&listens, "listen", "listen=target: accept connections at listen here and carry them to target on the hub (repeatable)")
	flags.Var(&allowed, "allow", "host:port on this machine the hub may connect to (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected link arguments")
	}
	forwards := make([]sshconnect.PortForward, 0, len(listens))
	for _, text := range listens {
		forward, err := sshconnect.ParseForward(text)
		if err != nil {
			return err
		}
		forwards = append(forwards, forward)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return sshconnect.ServeLink(ctx, os.Stdin, os.Stdout, forwards, allowed)
}

type repeatedFlag []string

func (f *repeatedFlag) String() string { return "" }

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}
