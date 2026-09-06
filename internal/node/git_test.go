package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestOldGitIsAdvertisedWithoutRefusingStartup(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf 'git version 2.37.9\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	// Advertise is also the hub's probe, so both roles report the same shape.
	hub := Advertise("hub", nil, nil)
	registry, _ := artifactNode(t)
	remote, err := registry.Advert(t.Context(), "n")
	if err != nil {
		t.Fatalf("old git refused startup: %v", err)
	}
	for _, adv := range []nodewire.Advert{hub, remote} {
		if adv.Git != "2.37.9" || adv.GitMinimum != "2.38" || adv.GitWarning == "" || adv.Refused != "" {
			t.Fatalf("git advert = %+v", adv)
		}
	}
	if err := os.Remove(filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	refreshed, err := registry.Refresh(t.Context(), "n")
	if err != nil || refreshed.Git != "" || refreshed.GitWarning == "" {
		t.Fatalf("missing git refresh = %+v, %v", refreshed, err)
	}
}
