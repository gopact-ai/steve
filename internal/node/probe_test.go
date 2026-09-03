package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
)

// A launch check tells a binary that starts from one that cannot: a script
// that answers --version is launchable and names its version; one that
// exits non-zero still launched; an executable the loader cannot run did
// not; and a file without execute permission did not.
func TestLaunchTellsARunnableFromABrokenBinary(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good", "#!/bin/sh\necho 'good version 1.2.3'\n", 0o755)
	grumpy := write("grumpy", "#!/bin/sh\necho 'usage: grumpy [flags]' >&2\nexit 2\n", 0o755)
	broken := write("broken", "\x7fELF\x00\x00garbage that is not a program\n", 0o755)
	noexec := write("noexec", "#!/bin/sh\necho hi\n", 0o644)
	slow := write("slow", "#!/bin/sh\nsleep 30\n", 0o755)

	ctx := context.Background()
	if r := Launch(ctx, good); !r.OK || r.Version == nil || r.Version.Value != "1.2.3" || r.Version.Scheme != "semver" {
		t.Fatalf("good = %+v", r)
	}
	if r := Launch(ctx, grumpy); !r.OK || r.Result != "usage: grumpy [flags]" {
		t.Fatalf("a usage error is still a launch: %+v", r)
	}
	if r := Launch(ctx, broken); r.OK {
		t.Fatalf("garbage launched: %+v", r)
	}
	if r := Launch(ctx, noexec); r.OK {
		t.Fatalf("a non-executable launched: %+v", r)
	}
	old := LaunchTimeout
	_ = old
	started := time.Now()
	if r := Launch(ctx, slow); !r.OK || time.Since(started) > LaunchTimeout+2*time.Second {
		t.Fatalf("a program that starts but never answers is launchable, within the timeout: %+v after %s", r, time.Since(started))
	}
}

// The snapshot carries the launch check as its own evidence: a tool on
// PATH that starts is launchable with its version; one that does not start
// is unavailable, and says so.
func TestSnapshotCarriesLaunchEvidence(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "goodtool")
	broken := filepath.Join(dir, "brokentool")
	_ = os.WriteFile(good, []byte("#!/bin/sh\necho 'goodtool 2.0.0'\n"), 0o755)
	_ = os.WriteFile(broken, []byte("\x7fELFgarbage\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	probe := NewLaunchProbe()
	probe.pass(context.Background(), []string{"goodtool", "brokentool", "sh"})
	snap := Snapshot("n", 1, 1, Observe{Tools: []string{"goodtool", "brokentool", "sh"}, Launch: probe.Lookup})
	if snap == nil {
		t.Fatal("no snapshot")
	}
	byID := map[string]ability.Capability{}
	for _, c := range snap.Offers {
		if c.Kind == ability.Tool {
			byID[c.ID] = c
		}
	}
	if g := byID["goodtool"]; g.Availability != ability.Available || g.Assurance != ability.Launchable || g.Version == nil || g.Version.Value != "2.0.0" {
		t.Fatalf("goodtool = %+v", g)
	}
	if b := byID["brokentool"]; b.Availability != ability.Unavailable || b.Assurance != ability.Existence || b.Detail == "" {
		t.Fatalf("brokentool = %+v", b)
	}
	if s := byID["sh"]; s.Availability != ability.Available || s.Assurance != ability.Launchable {
		t.Fatalf("sh = %+v", s)
	}
	// Without a probe, existence is all the snapshot claims.
	plain := Snapshot("n", 1, 2, Observe{Tools: []string{"goodtool"}})
	if plain.Offers[0].Assurance != ability.Existence || len(plain.Offers[0].Evidence) != 1 {
		t.Fatalf("without a probe = %+v", plain.Offers[0])
	}
}
