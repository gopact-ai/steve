package sshconnect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "config")
}

func TestDiscoverIncludesExplicitAliasesAndFirstValueWins(t *testing.T) {
	path := configFixture(t, map[string]string{
		"config": `# hosts that are already configured
Include "conf.d/*.conf"
Host dev secondary *.wild !excluded
    HostName ignored.example
    User ignored
Host *
    User fallback
    Port 2222
    IdentityFile ~/.ssh/private-key
`,
		"conf.d/01.conf": `Host dev
    HostName=dev.example
    User "developer"
    ProxyJump jump.example
Include conf.d/02.conf
`,
		"conf.d/02.conf": `Host secondary
    HostName 192.0.2.2
    ProxyCommand ssh jump -W %h:%p --secret=do-not-display
`,
	})
	d, err := Discover(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 2 {
		t.Fatalf("candidates = %#v", d.Candidates)
	}
	dev, secondary := d.Candidates[0], d.Candidates[1]
	if dev.Alias != "dev" || dev.HostName != "dev.example" || dev.User != "developer" || dev.Port != 2222 || dev.ProxyJump != "jump.example" || !dev.HasIdentityFile {
		t.Fatalf("dev = %#v", dev)
	}
	if secondary.Alias != "secondary" || secondary.HostName != "192.0.2.2" || !secondary.HasProxyCommand {
		t.Fatalf("secondary = %#v", secondary)
	}
	if strings.Contains(d.String(), "do-not-display") || strings.Contains(d.String(), "private-key") {
		t.Fatal("discovery leaked authentication configuration")
	}
}

func TestDiscoverBoundsCyclesAndDoesNotEvaluateMatch(t *testing.T) {
	path := configFixture(t, map[string]string{
		"config": `Include more
Host safe
    HostName safe.example
Match exec "touch /must-not-run"
    HostName conditional.example
    ProxyCommand secrets-must-not-be-displayed
Host after
    HostName after.example
`,
		"more": `Include config
Host included
    User include-user
`,
	})
	d, err := Discover(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 3 || len(d.Warnings) == 0 {
		t.Fatalf("discovery = %#v", d)
	}
	for _, c := range d.Candidates {
		if c.Alias == "safe" && c.HostName != "safe.example" {
			t.Fatalf("Match overrode static host data: %#v", c)
		}
		if !c.Conditional {
			t.Fatal("conditional config should be disclosed before connecting")
		}
	}
	if strings.Contains(d.String(), "secrets-must") || strings.Contains(d.String(), "touch") {
		t.Fatal("Match or ProxyCommand content leaked")
	}
}

func TestDiscoverIgnoresUnsafeNamesAndMissingConfiguration(t *testing.T) {
	path := configFixture(t, map[string]string{"config": `Host normal "-oProxyCommand=evil" "bad;name" bad/name user@host "space name" *.example !negated
    HostName normal.example
`})
	d, err := Discover(context.Background(), path)
	if err != nil || len(d.Candidates) != 1 || d.Candidates[0].Alias != "normal" {
		t.Fatalf("unsafe candidates: %#v, %v", d, err)
	}
	d, err = Discover(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err != nil || d.Candidates == nil || len(d.Candidates) != 0 {
		t.Fatalf("missing config = %#v, %v", d, err)
	}
}

func TestDiscoverIncludesAreReadAsRegularFilesAndRespectCancellation(t *testing.T) {
	path := configFixture(t, map[string]string{"config": "Include folder/*\nHost main\n"})
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "folder", "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := Discover(context.Background(), path)
	if err != nil || len(d.Candidates) != 1 || len(d.Warnings) != 1 {
		t.Fatalf("nonregular include = %#v, %v", d, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, path); err != context.Canceled {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestDiscoverRevisionChangesWithIncludedConfig(t *testing.T) {
	path := configFixture(t, map[string]string{"config": "Include included\n", "included": "Host dev\nHostName first.example\n"})
	before, err := Discover(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "included"), []byte("Host dev\nHostName second.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := Discover(context.Background(), path)
	if err != nil || before.Revision == after.Revision {
		t.Fatalf("included change did not invalidate revision: %v", err)
	}
}

func TestDiscoveryProxyFirstSettingMatchesOpenSSHPrecedence(t *testing.T) {
	path := configFixture(t, map[string]string{"config": "Host dev\n ProxyCommand example --credential secret\n ProxyJump ignored.example\n"})
	d, err := Discover(t.Context(), path)
	if err != nil || len(d.Candidates) != 1 || !d.Candidates[0].HasProxyCommand || d.Candidates[0].ProxyJump != "" {
		t.Fatalf("proxy precedence = %#v, %v", d, err)
	}
}
