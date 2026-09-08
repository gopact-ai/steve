package main

import (
	"flag"
	"time"

	"github.com/gopact-ai/steve/internal/app"
)

func doctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	timeout := flags.Duration("timeout", 2*time.Minute, "total harness probe timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return app.Doctor(*configPath, *timeout)
}
