package app

import (
	"os"
	"testing"
)

func TestLocalNodeNamePrefersTheClusterIdentity(t *testing.T) {
	t.Setenv("STEVE_NODE", "configured")
	if got := localNodeName(&Environment{NodeID: "node-a"}); got != "node-a" {
		t.Fatalf("cluster member named %q", got)
	}
	if got := localNodeName(&Environment{}); got != "configured" {
		t.Fatalf("environment without identity named %q", got)
	}
	if got := localNodeName(nil); got != "configured" {
		t.Fatalf("standalone named %q", got)
	}
	t.Setenv("STEVE_NODE", " ")
	want, err := os.Hostname()
	if err != nil || want == "" {
		want = "local"
	}
	if got := localNodeName(nil); got != want {
		t.Fatalf("standalone without STEVE_NODE named %q, want %q", got, want)
	}
}
