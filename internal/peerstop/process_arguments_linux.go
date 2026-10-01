//go:build linux

package peerstop

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Match the peer command's flag syntax without choosing between repeated
// identity arguments. An ambiguous installed peer is not proof of absence.
func peerConfiguration(args []string) (string, string, error) {
	flags := flag.NewFlagSet("peer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := uniquePathFlag{value: "config.json"}
	sidecar := uniquePathFlag{allowEmpty: true}
	flags.Var(&config, "config", "application configuration")
	flags.Var(&sidecar, "cluster-config", "cluster configuration")
	if err := flags.Parse(args); err != nil {
		return "", "", fmt.Errorf("%w: peer configuration arguments: %v", ErrUnproven, err)
	}
	if flags.NArg() != 0 {
		return "", "", fmt.Errorf("%w: unexpected peer arguments", ErrUnproven)
	}
	if sidecar.value == "" {
		sidecar.value = config.value + ".cluster.json"
	}
	return config.value, sidecar.value, nil
}

type uniquePathFlag struct {
	value      string
	set        bool
	allowEmpty bool
}

func (f *uniquePathFlag) String() string { return f.value }
func (f *uniquePathFlag) Set(value string) error {
	if f.set || value == "" && !f.allowEmpty || strings.HasPrefix(value, "-") {
		return errors.New("configuration path is repeated, missing or ambiguous")
	}
	f.value, f.set = value, true
	return nil
}
