package nodebootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the peer program: started as `steve peer` it
// waits for SIGTERM the way a peer would, so the upgrade script has a real
// process, with a real command line, to stop and restart. With
// STEVE_NODEBOOTSTRAP_STUB_LINGER it takes that long to exit once asked,
// as a peer winding down its executions does. With
// STEVE_NODEBOOTSTRAP_STUB_LOCK it first takes its installation's gateway
// lock, as a peer does, and holds it until it exits.
func TestMain(m *testing.M) {
	if os.Getenv("STEVE_NODEBOOTSTRAP_STUB") == "1" && len(os.Args) > 1 && os.Args[1] == "peer" {
		var lock *os.File
		if os.Getenv("STEVE_NODEBOOTSTRAP_STUB_LOCK") == "1" {
			lock = holdGatewayLock()
		}
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM)
		<-stop
		if linger, err := time.ParseDuration(os.Getenv("STEVE_NODEBOOTSTRAP_STUB_LINGER")); err == nil {
			time.Sleep(linger)
		}
		runtime.KeepAlive(lock)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// lockingStub is the environment that makes the stub peer take its
// installation's gateway lock.
const lockingStub = "STEVE_NODEBOOTSTRAP_STUB_LOCK=1"

// holdGatewayLock takes the gateway lock beside the configuration the
// stub peer was started with, the way a peer takes it, and records the
// stub's pid in it. When another process holds the lock the stub exits
// with the message a peer exits with.
func holdGatewayLock() *os.File {
	config := ""
	for i, arg := range os.Args {
		if arg == "--config" && i+1 < len(os.Args) {
			config = os.Args[i+1]
		}
	}
	dir := filepath.Join(filepath.Dir(config), "cluster", "peer-process")
	path := filepath.Join(dir, "gateway.lock")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr, "steve: another gateway already serves %s (lock %s is held)\n", dir, path)
		os.Exit(1)
	}
	if err := file.Truncate(0); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return file
}

func upgradeSpec() UpgradeSpec {
	return UpgradeSpec{UploadID: strings.Repeat("c", 48), OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: strings.Repeat("d", 64)}
}

func TestPeerUpgradeScriptRequiresVerifiedInputs(t *testing.T) {
	script, err := BuildPeerUpgrade(upgradeSpec())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "steve.previous") || !strings.Contains(script, "peer --config") || strings.Contains(script, "peer-import") {
		t.Fatal("upgrade script must swap the program and restart the peer without importing state again")
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid script %s %v", output, err)
	}
	for _, bad := range []UpgradeSpec{{UploadID: "../../bad", OS: "linux", Arch: "amd64", SHA256: strings.Repeat("d", 64)}, {UploadID: strings.Repeat("c", 48), OS: "plan9", Arch: "amd64", SHA256: strings.Repeat("d", 64)}, {UploadID: strings.Repeat("c", 48), OS: "linux", Arch: "amd64", SHA256: "short"}} {
		if _, err := BuildPeerUpgrade(bad); err == nil {
			t.Fatalf("invalid upgrade data accepted: %+v", bad)
		}
	}
}

// layoutPeer lays out ~/.steve-peer with the test binary as the peer
// program, the way the install script leaves it, without starting it.
func layoutPeer(t *testing.T) (home string, program []byte) {
	t.Helper()
	home = t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err = os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, ".steve-peer")
	if err := os.MkdirAll(filepath.Join(state, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "bin", "steve"), program, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "steve-bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("pkill", "-KILL", "-f", "^"+state+"/bin/steve peer ").Run() })
	return home, program
}

// installedPeer lays out ~/.steve-peer and starts the peer from it.
func installedPeer(t *testing.T) (home string, pid string) {
	t.Helper()
	home, _ = layoutPeer(t)
	return home, startPeer(t, home)
}

// startPeer starts the installed program of the installation under home
// as its peer, with env added to the test's environment, and returns its
// pid.
func startPeer(t *testing.T, home string, env ...string) string {
	t.Helper()
	start := exec.Command("bash", "-c", `nohup "$HOME/.steve-peer/bin/steve" peer --config "$HOME/.steve-peer/config.json" >> "$HOME/.steve-peer/peer.log" 2>&1 < /dev/null & echo $!`)
	start.Env = append(append(os.Environ(), "HOME="+home, "STEVE_NODEBOOTSTRAP_STUB=1"), env...)
	out, err := start.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func stageUpload(t *testing.T, home, id string, content []byte) string {
	t.Helper()
	dir := filepath.Join(home, "steve-bin", ".upload-"+id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "steve-node"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// runUpgrade runs the upgrade script for the installation under home,
// with env added to the test's environment.
func runUpgrade(t *testing.T, home string, spec UpgradeSpec, env ...string) (string, error) {
	t.Helper()
	script, err := BuildPeerUpgrade(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(append(os.Environ(), "HOME="+home, "STEVE_NODEBOOTSTRAP_STUB=1"), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func peerPIDs(t *testing.T, home string) []string {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", "^"+filepath.Join(home, ".steve-peer")+"/bin/steve peer ").Output()
	return strings.Fields(string(out))
}

func TestPeerUpgradeScriptSwapsTheProgramAndRestartsThePeer(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("peer upgrade runs on linux and darwin")
	}
	home, oldPID := installedPeer(t)
	program, _ := os.ReadFile(filepath.Join(home, ".steve-peer", "bin", "steve"))
	spec := upgradeSpec()
	spec.SHA256 = stageUpload(t, home, spec.UploadID, program)
	out, err := runUpgrade(t, home, spec)
	if err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".steve-peer", "bin", "steve.previous")); err != nil {
		t.Fatal("the previous program was not kept")
	}
	if _, err := os.Stat(filepath.Join(home, "steve-bin", ".upload-"+spec.UploadID)); !os.IsNotExist(err) {
		t.Fatal("the upload staging directory was left behind")
	}
	if pids := peerPIDs(t, home); len(pids) != 1 || pids[0] == oldPID {
		t.Fatalf("expected exactly one new peer process, old %s, got %v\n%s", oldPID, pids, out)
	}
	if !strings.Contains(out, "Stopping peer process "+oldPID) {
		t.Fatalf("the running peer was not the one stopped:\n%s", out)
	}
}

func TestPeerUpgradeScriptRestoresThePreviousProgramWhenTheNewOneExits(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("peer upgrade runs on linux and darwin")
	}
	home, oldPID := installedPeer(t)
	program, _ := os.ReadFile(filepath.Join(home, ".steve-peer", "bin", "steve"))
	spec := upgradeSpec()
	spec.SHA256 = stageUpload(t, home, spec.UploadID, []byte("#!/bin/sh\nexit 1\n"))
	out, err := runUpgrade(t, home, spec)
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 26 {
		t.Fatalf("expected exit 26 after rollback, got %v\n%s", err, out)
	}
	restored, _ := os.ReadFile(filepath.Join(home, ".steve-peer", "bin", "steve"))
	if string(restored) != string(program) {
		t.Fatal("the previous program was not put back")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pids := peerPIDs(t, home)
		if len(pids) == 1 && pids[0] != oldPID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the previous program was not restarted: old %s, running %v\n%s", oldPID, pids, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// After an upgrade whose program died later than the script watched, the
// machine has no peer running, a broken program installed and the last
// working one as steve.previous. Upgrading again must not rotate the
// broken program into steve.previous: when the next program fails too,
// the machine comes back on the one that worked.
func TestPeerUpgradeScriptKeepsTheLastWorkingProgramWhenNoPeerRuns(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("peer upgrade runs on linux and darwin")
	}
	home, program := layoutPeer(t)
	bin := filepath.Join(home, ".steve-peer", "bin")
	if err := os.Rename(filepath.Join(bin, "steve"), filepath.Join(bin, "steve.previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "steve"), []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := upgradeSpec()
	spec.SHA256 = stageUpload(t, home, spec.UploadID, []byte("#!/bin/sh\nexit 1\n"))
	out, err := runUpgrade(t, home, spec)
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 26 {
		t.Fatalf("expected exit 26 after falling back to the working program, got %v\n%s", err, out)
	}
	if restored, _ := os.ReadFile(filepath.Join(bin, "steve")); string(restored) != string(program) {
		t.Fatalf("the last working program was not put back:\n%s", out)
	}
	if !strings.Contains(out, "No peer is running") {
		t.Fatalf("the script did not say why the installed program was set aside:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(bin, "steve.new")); !os.IsNotExist(err) {
		t.Fatal("the staged program was left behind")
	}
}

// When neither the new program nor the one it falls back to stays up, the
// peer is down: the fallback is left installed as steve, the new program
// set aside as steve.rejected, and nothing else, neither steve.previous
// nor a staged steve.new, is left beside them.
func TestPeerUpgradeScriptLeftDownKeepsTheFallbackInstalledAndTheNewProgramRejected(t *testing.T) {
	requirePeerPlatform(t)
	home, _ := layoutPeer(t)
	bin := filepath.Join(home, ".steve-peer", "bin")
	fallback, installed, upgraded := "#!/bin/sh\nexit 4\n", "#!/bin/sh\nexit 3\n", "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "steve.previous"), []byte(fallback), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "steve"), []byte(installed), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := upgradeSpec()
	spec.SHA256 = stageUpload(t, home, spec.UploadID, []byte(upgraded))
	out, err := runUpgrade(t, home, spec)
	if exitCode(err) != 28 {
		t.Fatalf("expected exit 28 with the peer down, got %v\n%s", err, out)
	}
	if left := programs(t, home); !maps.Equal(left, map[string]string{"steve": fallback, "steve.rejected": upgraded}) {
		t.Fatalf("an upgrade left down left %v\n%s", slices.Sorted(maps.Keys(left)), out)
	}
}

func TestPeerUpgradeScriptRefusesAMachineWithoutAPeer(t *testing.T) {
	home := t.TempDir()
	out, err := runUpgrade(t, home, upgradeSpec())
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 30 {
		t.Fatalf("expected exit 30, got %v\n%s", err, out)
	}
}

// A peer started from its own directory as ./bin/steve is the same peer.
// Missing it leaves the old process holding the gateway lock, the new
// program exits on it, and a working upgrade looks like a broken build.
func TestPeerUpgradeScriptStopsAPeerStartedByARelativePath(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("peer upgrade runs on linux and darwin")
	}
	home, program := layoutPeer(t)
	state := filepath.Join(home, ".steve-peer")
	start := exec.Command("bash", "-c", `cd "$HOME/.steve-peer"; nohup ./bin/steve peer --config ./config.json >> peer.log 2>&1 < /dev/null & echo $!`)
	start.Env = append(os.Environ(), "HOME="+home, "STEVE_NODEBOOTSTRAP_STUB=1")
	out, err := start.Output()
	if err != nil {
		t.Fatal(err)
	}
	oldPID := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("kill", "-KILL", oldPID).Run() })
	if err := os.MkdirAll(filepath.Join(state, "cluster", "peer-process"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "cluster", "peer-process", "gateway.lock"), []byte(oldPID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := upgradeSpec()
	spec.SHA256 = stageUpload(t, home, spec.UploadID, program)
	report, err := runUpgrade(t, home, spec)
	if err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, report)
	}
	if !strings.Contains(report, "Stopping peer process "+oldPID) {
		t.Fatalf("the relatively started peer was not stopped:\n%s", report)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if exec.Command("kill", "-0", oldPID).Run() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the old peer kept running through the upgrade:\n%s", report)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A peer the script cannot find, where the gateway lock does not tell who
// holds it, keeps holding the lock, and the new program exits on it. That
// program is not the one at fault and the one that ran is not down: the
// upgrade puts the installed programs back as they were, starts nothing
// beside the peer that runs, says that peer was not stopped and the
// machine not upgraded, and exits 31.
func TestPeerUpgradeScriptPutsTheProgramsBackWhenThePeerWasNotStopped(t *testing.T) {
	requirePeerPlatform(t)
	for _, earlier := range []bool{false, true} {
		real, home := symlinkedPeer(t)
		bin := filepath.Join(real, ".steve-peer", "bin")
		if earlier {
			if err := os.WriteFile(filepath.Join(bin, "steve.previous"), []byte("earlier program"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		pid := spelledPeer(t, real, home, throughPath)
		lockedBy(t, real, "")
		before := programs(t, home)
		running, _ := os.ReadFile(filepath.Join(bin, "steve"))
		spec := upgradeSpec()
		spec.SHA256 = stageUpload(t, home, spec.UploadID, append(slices.Clone(running), "\nnew program\n"...))
		report, err := runUpgrade(t, home, spec, lockingStub)
		if exitCode(err) != 31 || !strings.Contains(report, "was not stopped") || !strings.Contains(report, "not upgraded") || !strings.Contains(report, "another gateway already serves") {
			t.Fatalf("earlier program %v: expected exit 31 saying the peer was not stopped and the machine not upgraded, got %v\n%s", earlier, err, report)
		}
		if after := programs(t, home); !maps.Equal(before, after) {
			t.Fatalf("earlier program %v: the programs were not put back: before %v, after %v\n%s", earlier, slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)), report)
		}
		if exec.Command("kill", "-0", pid).Run() != nil {
			t.Fatalf("earlier program %v: the peer %s holding the lock was stopped:\n%s", earlier, pid, report)
		}
		if pids := peerPIDs(t, home); len(pids) != 0 {
			t.Fatalf("earlier program %v: a program was left running beside the peer: %v\n%s", earlier, pids, report)
		}
	}
}

// An upgrade that cannot look for the peer exits 29 as a restart does,
// and leaves the machine as it found it: the programs as they were, no
// staged program beside them, no upload and no installation lock.
func TestPeerUpgradeScriptLeavesTheMachineAsItWasWhereItCannotLookForThePeer(t *testing.T) {
	requirePeerPlatform(t)
	for name, spoil := range unsearchable(t) {
		t.Run(name, func(t *testing.T) {
			home, pid := installedPeer(t)
			before := programs(t, home)
			running, _ := os.ReadFile(filepath.Join(home, ".steve-peer", "bin", "steve"))
			spec := upgradeSpec()
			spec.SHA256 = stageUpload(t, home, spec.UploadID, append(slices.Clone(running), "\nnew program\n"...))
			report, err := runUpgrade(t, home, spec, spoil(t, home, pid)...)
			if exitCode(err) != 29 || strings.Contains(report, "Stopping peer process") || !strings.Contains(report, "nothing was stopped or started") {
				t.Fatalf("expected exit 29 with nothing stopped or started, got %v\n%s", err, report)
			}
			if after := programs(t, home); !maps.Equal(before, after) {
				t.Fatalf("the programs were not left as they were: before %v, after %v\n%s", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)), report)
			}
			left, err := os.ReadDir(filepath.Join(home, "steve-bin"))
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Fatalf("the upload or the installation lock was left behind: %v\n%s", left, report)
			}
			if pids := peerPIDs(t, home); len(pids) != 1 || pids[0] != pid {
				t.Fatalf("the running peer %s was touched: %v\n%s", pid, pids, report)
			}
		})
	}
}
