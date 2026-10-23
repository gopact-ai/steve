package nodebootstrap

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAutomaticRestartTracksItsOwnChildRatherThanANumericPID(t *testing.T) {
	requirePeerPlatform(t)
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "child-exited", true: "child-running"}[live], func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, ".steve-peer")
			if err := os.MkdirAll(filepath.Join(state, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "config.json"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			program := `#!/bin/bash
case "$1" in
 --peer-stop-protocol) printf 'STEVE_PEER_STOP_V1\n'; exit 0 ;;
 peer-stop) printf 'STEVE_PEER_STOP\tabsent\n'; exit 0 ;;
 peer) if [ "$FIXTURE_LIVE" = 1 ]; then exec sh -c 'read -r line <&3; exit 0'; fi; exit 0 ;;
esac
exit 2
`
			if err := os.WriteFile(filepath.Join(state, "bin/steve"), []byte(program), 0700); err != nil {
				t.Fatal(err)
			}
			envfile := filepath.Join(home, "shell-env")
			// A numeric liveness observation can be true after the actual child is
			// gone. This is an observation counterexample, not a forced kernel reuse.
			if err := os.WriteFile(envfile, []byte("kill() { if [ \"$1\" = -0 ]; then return 0; fi; return 99; }\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			script := BuildPeerRestart(RestartSpec{ExpectedCluster: "cluster", ExpectedNode: "node"})
			// Keep the launcher around to reap its own fixture after observing success.
			script = strings.Replace(script, "  exit 0\nfi\nif exited_on_lock", "  wait\n  exit 0\nfi\nif exited_on_lock", 1)
			cmd := exec.CommandContext(ctx, "bash", "-s")
			cmd.Stdin = strings.NewReader(script)
			cmd.ExtraFiles = []*os.File{read}
			cmd.Env = append(os.Environ(), "HOME="+home, "BASH_ENV="+envfile, "FIXTURE_LIVE="+map[bool]string{false: "0", true: "1"}[live])
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			read.Close()
			var release sync.Once
			releaseChild := func() { release.Do(func() { write.Close() }) }
			done := make(chan struct {
				out string
				err error
			}, 1)
			go func() {
				var output strings.Builder
				scanner := bufio.NewScanner(pipe)
				for scanner.Scan() {
					line := scanner.Text()
					output.WriteString(line + "\n")
					if strings.HasPrefix(line, "STEVE_RESTART\t") {
						releaseChild()
					}
				}
				releaseChild()
				done <- struct {
					out string
					err error
				}{output.String(), cmd.Wait()}
			}()
			var result struct {
				out string
				err error
			}
			select {
			case result = <-done:
			case <-ctx.Done():
				releaseChild()
				cmd.Process.Kill()
				<-done
				t.Fatal(ctx.Err())
			}
			if live {
				if result.err != nil || !strings.Contains(result.out, "STEVE_RESTART\tstarted") {
					t.Fatalf("own live child rejected: %v %s", result.err, result.out)
				}
			} else if result.err == nil || strings.Contains(result.out, "STEVE_RESTART\tstarted") {
				t.Fatalf("a departed child was replaced by numeric liveness: %v %s", result.err, result.out)
			}
		})
	}
}
