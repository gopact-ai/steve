package main

import (
	"errors"
	"flag"
	"testing"
)

func TestPluginsCommandIsRegisteredWithoutStartingTheHub(t *testing.T) {
	if _, ok := commands["plugins"]; !ok {
		t.Fatal("plugins command is not registered")
	}
	if err := run([]string{"plugins", "preview", "-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("plugins dispatch: %v", err)
	}
}
