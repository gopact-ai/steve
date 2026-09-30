package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitInitializationFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", filepath.Base(path), ctx.Err())
		case <-tick.C:
		}
	}
}

func TestOpenFromAnotherProcess(t *testing.T) {
	root, id := os.Getenv("GITREPO_PROCESS_ROOT"), os.Getenv("GITREPO_PROCESS_ID")
	if root == "" || id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	write(t, root, "ready-"+id, "ready")
	waitInitializationFile(t, ctx, filepath.Join(root, "start"))
	repo, err := Open(ctx, filepath.Join(root, "objects", "p.git"))
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work-"+id)
	write(t, work, "answer", id)
	sha, _, err := repo.Snapshot(ctx, work, "", "concurrent initialization", false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "result-"+id, sha)
}

func TestOpenPublishesOneRepositoryAcrossProcessesWithoutLosingArtifacts(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var results []<-chan error
	var outputs []*bytes.Buffer
	defer func() {
		cancel()
		for _, done := range results {
			<-done
		}
	}()
	for i := range 3 {
		id := fmt.Sprint(i)
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestOpenFromAnotherProcess$")
		cmd.Env = append(os.Environ(), "GITREPO_PROCESS_ROOT="+root, "GITREPO_PROCESS_ID="+id)
		out := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait(); close(done) }()
		results, outputs = append(results, done), append(outputs, out)
	}
	for i := range 3 {
		waitInitializationFile(t, ctx, filepath.Join(root, fmt.Sprintf("ready-%d", i)))
	}
	write(t, root, "start", "start")
	for i, done := range results {
		if err := <-done; err != nil {
			t.Errorf("initializer %d: %v: %s", i, err, outputs[i].String())
		}
	}
	if t.Failed() {
		return
	}
	repo, err := Open(ctx, filepath.Join(root, "objects", "p.git"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		sha := strings.TrimSpace(read(t, root, fmt.Sprintf("result-%d", i)))
		if !ValidSHA(sha) || !repo.Has(ctx, sha) {
			t.Fatalf("initializer %d lost its artifact %q", i, sha)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "objects"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "p.git" {
		t.Fatalf("initializers left private staging directories: %v, %v", entries, err)
	}
}
