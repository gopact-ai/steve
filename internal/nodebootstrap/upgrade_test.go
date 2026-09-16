package nodebootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the peer program: started as `steve peer` it
// waits for SIGTERM the way a peer would, so the upgrade script has a real
// process, with a real command line, to stop and restart.
func TestMain(m *testing.M) {
	if os.Getenv("STEVE_NODEBOOTSTRAP_STUB") == "1" && len(os.Args) > 1 && os.Args[1] == "peer" {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM)
		<-stop
		os.Exit(0)
	}
	os.Exit(m.Run())
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
	start := exec.Command("bash", "-c", `nohup "$HOME/.steve-peer/bin/steve" peer --config "$HOME/.steve-peer/config.json" >> "$HOME/.steve-peer/peer.log" 2>&1 < /dev/null & echo $!`)
	start.Env = append(os.Environ(), "HOME="+home, "STEVE_NODEBOOTSTRAP_STUB=1")
	out, err := start.Output()
	if err != nil {
		t.Fatal(err)
	}
	return home, strings.TrimSpace(string(out))
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

func runUpgrade(t *testing.T, home string, spec UpgradeSpec) (string, error) {
	t.Helper()
	script, err := BuildPeerUpgrade(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+home, "STEVE_NODEBOOTSTRAP_STUB=1")
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

func TestPeerUpgradeScriptRefusesAMachineWithoutAPeer(t *testing.T) {
	home := t.TempDir()
	out, err := runUpgrade(t, home, upgradeSpec())
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 30 {
		t.Fatalf("expected exit 30, got %v\n%s", err, out)
	}
}
