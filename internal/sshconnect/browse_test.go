package sshconnect

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// browseShellRunner runs whatever browse script the service sends through a
// real shell with a fixture HOME, the way the remote account would.
type browseShellRunner struct {
	home string
	sent []string
}

func (r *browseShellRunner) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	return &fixtureConnection{runner: r, args: args}, nil
}
func (r *browseShellRunner) Run(ctx context.Context, args []string, input string) (Output, error) {
	if args[len(args)-1] != "sh -s" {
		return Output{}, context.Canceled
	}
	r.sent = append(r.sent, input)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-s")
	cmd.Env = []string{"HOME=" + r.home, "PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	return Output{Stdout: string(out)}, err
}
func (r *browseShellRunner) Upload(context.Context, []string, io.Reader) (Output, error) {
	return Output{}, context.Canceled
}

func TestBrowseListsRemoteDirectoriesRelativeToTheAccountHome(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{"work/steve", "work/other", "Projects", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := configFixture(t, map[string]string{"config": "Host dev\nHostName dev.example\n"})
	svc := New(Options{ConfigPath: path, Runner: &browseShellRunner{home: home}, Backend: &fakeBackend{}})
	t.Cleanup(func() { _ = svc.Close() })

	listing, err := svc.Browse(t.Context(), BrowseRequest{Alias: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if listing.Path != home || listing.Display != "~" || listing.Home != home || !listing.Writable {
		t.Fatalf("home listing: %+v", listing)
	}
	var names []string
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
	}
	if strings.Join(names, ",") != "Projects,work" {
		t.Fatalf("home should list only visible directories: %v", names)
	}

	listing, err = svc.Browse(t.Context(), BrowseRequest{Alias: "dev", Path: "~/work"})
	if err != nil || listing.Display != "~/work" || listing.Parent != home || len(listing.Entries) != 2 || listing.Entries[1].Path != filepath.Join(home, "work", "steve") {
		t.Fatalf("~/work listing: %+v %v", listing, err)
	}

	listing, err = svc.Browse(t.Context(), BrowseRequest{Alias: "dev", Path: "/"})
	if err != nil || listing.Path != "/" || listing.Parent != "" {
		t.Fatalf("root listing: %+v %v", listing, err)
	}

	_, err = svc.Browse(t.Context(), BrowseRequest{Alias: "dev", Path: "~/does-not-exist"})
	var step *StepError
	if !asStep(err, &step) || step.Code != "directory_unavailable" {
		t.Fatalf("a missing directory is a finding, not a crash: %v", err)
	}
}

func TestBrowseRejectsPathsItWouldNotUseAsAWorkspace(t *testing.T) {
	runner := &browseShellRunner{home: t.TempDir()}
	path := configFixture(t, map[string]string{"config": "Host dev\nHostName dev.example\n"})
	svc := New(Options{ConfigPath: path, Runner: runner, Backend: &fakeBackend{}})
	t.Cleanup(func() { _ = svc.Close() })
	for _, bad := range []string{"relative", "~/../etc", "/tmp/a\nb", "/tmp/$(touch pwned)", "/tmp/`id`", "/tmp/'q'"} {
		_, err := svc.Browse(t.Context(), BrowseRequest{Alias: "dev", Path: bad})
		var step *StepError
		if bad == "/tmp/$(touch pwned)" || bad == "/tmp/`id`" || bad == "/tmp/'q'" {
			// Odd but legal names travel to the shell as data only.
			if err == nil || !asStep(err, &step) || step.Code != "directory_unavailable" {
				t.Fatalf("%q: %v", bad, err)
			}
			continue
		}
		if !asStep(err, &step) || step.Code != "invalid_path" {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	for _, sent := range runner.sent {
		if strings.Contains(sent, "touch pwned") || strings.Contains(sent, "`id`") {
			t.Fatalf("the path reached the shell as code: %s", sent)
		}
	}
	if _, err := os.Stat("pwned"); err == nil {
		t.Fatal("the path ran")
	}
}

func asStep(err error, step **StepError) bool { return errors.As(err, step) }
