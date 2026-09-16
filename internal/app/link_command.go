package app

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gopact-ai/steve/internal/sshconnect"
)

// LinkCommand is `steve link`, the far end of an SSH link: the hub runs it
// on this machine over the session it opened and multiplexes the cluster
// protocol through its stdin and stdout. It ends with the session.
func LinkCommand(args []string) error {
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
	for _, target := range allowed {
		if err := sshconnect.CheckAddress(target); err != nil {
			return err
		}
	}
	// What this command writes on stderr reaches the hub as the reason the
	// link ended; it wants no timestamps.
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return sshconnect.ServeLink(ctx, os.Stdin, os.Stdout, os.Stderr, forwards, allowed)
}

type repeatedFlag []string

func (f *repeatedFlag) String() string { return "" }

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}
