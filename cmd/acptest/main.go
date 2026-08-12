// acptest sends one prompt to the configured ACP agent backend from the
// terminal, bypassing Feishu. Useful to verify agent config before wiring
// up the channel: go run ./cmd/acptest -config config.yaml "hello"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"acpgw/internal/acphost"
	"acpgw/internal/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	timeout := flag.Duration("timeout", 10*time.Minute, "prompt timeout")
	flag.Parse()

	prompt := strings.Join(flag.Args(), " ")
	if prompt == "" {
		log.Fatal("usage: acptest [-config config.yaml] <prompt>")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("acptest: %v", err)
	}

	host := acphost.New(acphost.Config{
		Command:    cfg.Agent.Command,
		Args:       cfg.Agent.Args,
		Workdir:    cfg.Agent.Workdir,
		Env:        cfg.Agent.Env,
		Permission: cfg.Agent.Permission,
	})
	defer host.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	sid, err := host.NewChatSession(ctx)
	if err != nil {
		log.Fatalf("acptest: new session: %v", err)
	}
	log.Printf("acptest: session %s created, sending prompt...", sid)

	out, activity, err := host.Prompt(ctx, sid, prompt, func(line string) {
		log.Printf("acptest: %s", line)
	})
	if err != nil {
		log.Fatalf("acptest: prompt: %v", err)
	}
	for _, line := range activity {
		log.Printf("acptest: activity: %s", line)
	}
	fmt.Println(out)
}
