package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestPeerStopProtocolIsAnExplicitNonServingCapability(t *testing.T) {
	previous := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	defer func() { os.Stdout = previous; read.Close(); write.Close() }()
	err = run([]string{"--peer-stop-protocol"})
	write.Close()
	os.Stdout = previous
	raw, readErr := io.ReadAll(read)
	if err != nil || readErr != nil || strings.TrimSpace(string(raw)) != "STEVE_PEER_STOP_V1" {
		t.Fatalf("capability probe=%q err=%v read=%v", raw, err, readErr)
	}
}

func TestPeerStopCommandDoesNotAcceptAnArbitraryPID(t *testing.T) {
	command, ok := commands["peer-stop"]
	if !ok {
		t.Fatal("peer-stop command missing")
	}
	if err := command([]string{"--pid", "1234"}); err == nil {
		t.Fatal("arbitrary PID accepted")
	}
	if err := command([]string{"--cluster-config", "elsewhere.json", "--expect-cluster", "c"}); err == nil {
		t.Fatal("incomplete expected identity accepted")
	}
}
