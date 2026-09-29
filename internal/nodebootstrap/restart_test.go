package nodebootstrap

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// runRestart runs the restart script for the installation under home,
// with env added to the test's environment.
func runRestart(t *testing.T, home string, spec RestartSpec, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(BuildPeerRestart(spec))
	cmd.Env = append(append(os.Environ(), "HOME="+home, "STEVE_NODEBOOTSTRAP_STUB=1"), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// programs reads every program kept under ~/.steve-peer/bin, by name, so
// a test can tell that a restart left the installation exactly as it was.
func programs(t *testing.T, home string) map[string]string {
	t.Helper()
	bin := filepath.Join(home, ".steve-peer", "bin")
	entries, err := os.ReadDir(bin)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]string{}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(bin, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		kept[entry.Name()] = string(content)
	}
	return kept
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func requirePeerPlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("peer restart runs on linux and darwin")
	}
}

// Restarting a running peer stops the process the installation records
// and starts the installed program again. Nothing is uploaded and no
// program is moved: the same bytes run afterwards under a new process.
func TestPeerRestartScriptRestartsARunningPeer(t *testing.T) {
	requirePeerPlatform(t)
	home, oldPID := installedPeer(t)
	bin := filepath.Join(home, ".steve-peer", "bin")
	if err := os.WriteFile(filepath.Join(bin, "steve.previous"), []byte("previous program"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := programs(t, home)
	out, err := runRestart(t, home, RestartSpec{})
	if err != nil {
		t.Fatalf("restart failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopping peer process "+oldPID) || !strings.Contains(out, "STEVE_RESTART\trestarted\n") {
		t.Fatalf("the running peer was not reported stopped and restarted:\n%s", out)
	}
	if pids := peerPIDs(t, home); len(pids) != 1 || pids[0] == oldPID {
		t.Fatalf("expected exactly one new peer process, old %s, got %v\n%s", oldPID, pids, out)
	}
	if after := programs(t, home); !maps.Equal(before, after) {
		t.Fatalf("the restart moved or changed a program: before %v, after %v", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}
	if _, err := os.Stat(filepath.Join(home, "steve-bin", ".install-lock")); !os.IsNotExist(err) {
		t.Fatal("the installation lock was left behind")
	}
}

// A machine whose peer died has nothing to stop: the installed program is
// started as it is, and the script says the peer was started rather than
// restarted. An earlier fallback or rejected program stays where it is.
func TestPeerRestartScriptStartsAPeerThatIsNotRunning(t *testing.T) {
	requirePeerPlatform(t)
	home, _ := layoutPeer(t)
	bin := filepath.Join(home, ".steve-peer", "bin")
	if err := os.WriteFile(filepath.Join(bin, "steve.previous"), []byte("previous program"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "steve.rejected"), []byte("rejected program"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := programs(t, home)
	out, err := runRestart(t, home, RestartSpec{})
	if err != nil {
		t.Fatalf("start failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "Stopping peer process") || !strings.Contains(out, "STEVE_RESTART\tstarted\n") {
		t.Fatalf("a stopped peer was not reported started:\n%s", out)
	}
	if pids := peerPIDs(t, home); len(pids) != 1 {
		t.Fatalf("expected exactly one peer process, got %v\n%s", pids, out)
	}
	if after := programs(t, home); !maps.Equal(before, after) {
		t.Fatalf("the start moved or changed a program: before %v, after %v", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}
}

// A peer that exits right after it is started leaves the machine without
// one. The script says so, with the last lines the peer wrote to its log,
// and keeps the installed program in place for whoever looks next.
func TestPeerRestartScriptReportsAPeerThatDoesNotStayRunning(t *testing.T) {
	requirePeerPlatform(t)
	home, _ := layoutPeer(t)
	bin := filepath.Join(home, ".steve-peer", "bin")
	if err := os.WriteFile(filepath.Join(bin, "steve"), []byte("#!/bin/sh\necho 'config.json: cluster identity is unreadable' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := programs(t, home)
	out, err := runRestart(t, home, RestartSpec{})
	if exitCode(err) != 28 {
		t.Fatalf("expected exit 28, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "the peer is down") || !strings.Contains(out, "config.json: cluster identity is unreadable") || strings.Contains(out, "STEVE_RESTART") {
		t.Fatalf("the failed start was not reported with the peer's log:\n%s", out)
	}
	if after := programs(t, home); !maps.Equal(before, after) {
		t.Fatalf("the failed start moved or changed a program: before %v, after %v", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}
}

// Bringing up a peer only when it is down never touches one that still
// runs, however unreachable it is from the cluster: the script reports it
// running and leaves the process alone.
func TestPeerRestartScriptLeavesARunningPeerAloneWhenOnlyStartingAStoppedOne(t *testing.T) {
	requirePeerPlatform(t)
	home, oldPID := installedPeer(t)
	out, err := runRestart(t, home, RestartSpec{IfStopped: true})
	if err != nil {
		t.Fatalf("check failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "Stopping peer process") || !strings.Contains(out, "STEVE_RESTART\trunning\n") {
		t.Fatalf("a running peer was not reported running and left alone:\n%s", out)
	}
	if pids := peerPIDs(t, home); len(pids) != 1 || pids[0] != oldPID {
		t.Fatalf("the running peer %s was replaced: %v\n%s", oldPID, pids, out)
	}
}

// A peer the script does not find can still hold the gateway lock, and
// the program started beside it exits on that lock. Starting a peer only
// when it is down takes this for a peer that runs and leaves it alone; a
// restart says the peer that ran was not stopped, and exits 31. Only the
// line the program just wrote counts: an earlier run's line further up
// the log says nothing about this one.
func TestPeerRestartScriptTellsALockHeldByAPeerItDidNotFind(t *testing.T) {
	requirePeerPlatform(t)
	home, _ := layoutPeer(t)
	state := filepath.Join(home, ".steve-peer")
	held := "#!/bin/sh\necho \"steve: another gateway already serves $HOME/.steve-peer/cluster (lock $HOME/.steve-peer/cluster/peer-process/gateway.lock is held)\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(state, "bin", "steve"), []byte(held), 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := runRestart(t, home, RestartSpec{IfStopped: true})
	if err != nil || !strings.Contains(report, "STEVE_RESTART\trunning\n") {
		t.Fatalf("a peer holding the lock was not left alone as running: %v\n%s", err, report)
	}
	report, err = runRestart(t, home, RestartSpec{})
	if exitCode(err) != 31 || strings.Contains(report, "STEVE_RESTART") || !strings.Contains(report, "was not stopped") || !strings.Contains(report, "another gateway already serves") {
		t.Fatalf("expected exit 31 saying the running peer was not stopped, got %v\n%s", err, report)
	}
	unreadable := "#!/bin/sh\necho 'config.json: cluster identity is unreadable' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(state, "bin", "steve"), []byte(unreadable), 0o700); err != nil {
		t.Fatal(err)
	}
	report, err = runRestart(t, home, RestartSpec{IfStopped: true})
	if exitCode(err) != 28 || strings.Contains(report, "STEVE_RESTART") {
		t.Fatalf("an earlier line about the lock was taken for this start's: %v\n%s", err, report)
	}
}

// Bringing up a stopped peer when it is down starts it like a restart.
func TestPeerRestartScriptStartsAStoppedPeerWhenOnlyStartingAStoppedOne(t *testing.T) {
	requirePeerPlatform(t)
	home, _ := layoutPeer(t)
	out, err := runRestart(t, home, RestartSpec{IfStopped: true})
	if err != nil || !strings.Contains(out, "STEVE_RESTART\tstarted\n") {
		t.Fatalf("a stopped peer was not started: %v\n%s", err, out)
	}
	if pids := peerPIDs(t, home); len(pids) != 1 {
		t.Fatalf("expected exactly one peer process, got %v\n%s", pids, out)
	}
}

func TestPeerRestartScriptRefusesAMachineWithoutAPeer(t *testing.T) {
	home := t.TempDir()
	out, err := runRestart(t, home, RestartSpec{})
	if exitCode(err) != 30 {
		t.Fatalf("expected exit 30, got %v\n%s", err, out)
	}
}

// An installation or upgrade in progress holds the installation lock. A
// restart waits its turn rather than stopping a peer mid-swap, and the
// lock it did not take stays with its holder.
func TestPeerRestartScriptDefersToAnInstallationInProgress(t *testing.T) {
	requirePeerPlatform(t)
	home, oldPID := installedPeer(t)
	lock := filepath.Join(home, "steve-bin", ".install-lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := runRestart(t, home, RestartSpec{})
	if exitCode(err) != 21 {
		t.Fatalf("expected exit 21, got %v\n%s", err, out)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("the restart released a lock it did not hold")
	}
	if pids := peerPIDs(t, home); len(pids) != 1 || pids[0] != oldPID {
		t.Fatalf("the peer was touched while another installation held the lock: %v", pids)
	}
}

// A peer started from its own directory as ./bin/steve is found the way
// an upgrade finds it, through the pid its gateway lock records.
func TestPeerRestartScriptStopsAPeerStartedByARelativePath(t *testing.T) {
	requirePeerPlatform(t)
	home, _ := layoutPeer(t)
	oldPID := relativePeer(t, home)
	lockedBy(t, home, oldPID)
	report, err := runRestart(t, home, RestartSpec{})
	if err != nil {
		t.Fatalf("restart failed: %v\n%s", err, report)
	}
	if !strings.Contains(report, "Stopping peer process "+oldPID) || !strings.Contains(report, "STEVE_RESTART\trestarted\n") {
		t.Fatalf("the relatively started peer was not restarted:\n%s", report)
	}
	deadline := time.Now().Add(5 * time.Second)
	for exec.Command("kill", "-0", oldPID).Run() == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the old peer kept running through the restart:\n%s", report)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Every peer process of the installation is given its time to stop. One
// that exits at once does not let the stop pass over another still
// winding down, and the program is started again only once all are gone:
// started earlier, it would find the gateway lock still held.
func TestPeerRestartScriptWaitsForEveryPeerProcessItStops(t *testing.T) {
	requirePeerPlatform(t)
	home, quick := installedPeer(t)
	slow := startPeer(t, home, "STEVE_NODEBOOTSTRAP_STUB_LINGER=8s")
	report, err := runRestart(t, home, RestartSpec{})
	if err != nil || !strings.Contains(report, "STEVE_RESTART\trestarted\n") {
		t.Fatalf("restart failed: %v\n%s", err, report)
	}
	for _, pid := range []string{quick, slow} {
		if exec.Command("kill", "-0", pid).Run() == nil {
			t.Fatalf("the peer process %s was still running when the program was started again:\n%s", pid, report)
		}
	}
}

// The pid a gateway lock records outlives the peer that wrote it, and the
// number can come back as another process of the same account, here the
// peer of a second installation, started by its path or from its own
// directory and holding that installation's gateway lock. That peer
// belongs to its own installation: a restart or an upgrade of this one
// neither stops it nor takes it for this installation's peer, and starts
// this installation's own.
func TestPeerRestartScriptLeavesAnotherInstallationsPeerItsLockNames(t *testing.T) {
	requirePeerPlatform(t)
	byPath, _ := layoutPeer(t)
	fromItsDirectory, _ := layoutPeer(t)
	others := map[string]string{byPath: startPeer(t, byPath, lockingStub), fromItsDirectory: relativePeer(t, fromItsDirectory, lockingStub)}
	for other, otherPID := range others {
		awaitGatewayLock(t, other, otherPID)
		for _, spec := range []RestartSpec{{IfStopped: true}, {}} {
			home, _ := layoutPeer(t)
			lockedBy(t, home, otherPID)
			report, err := runRestart(t, home, spec, lockingStub)
			if err != nil || strings.Contains(report, "Stopping peer process") || !strings.Contains(report, "STEVE_RESTART\tstarted\n") {
				t.Fatalf("IfStopped %v: the peer %s of another installation was taken for this one's: %v\n%s", spec.IfStopped, otherPID, err, report)
			}
			if exec.Command("kill", "-0", otherPID).Run() != nil {
				t.Fatalf("IfStopped %v: the peer %s of the installation under %s was stopped:\n%s", spec.IfStopped, otherPID, other, report)
			}
			if pids := peerPIDs(t, home); len(pids) != 1 {
				t.Fatalf("IfStopped %v: expected this installation's peer to be started, got %v\n%s", spec.IfStopped, pids, report)
			}
		}
		home, program := layoutPeer(t)
		lockedBy(t, home, otherPID)
		spec := upgradeSpec()
		spec.SHA256 = stageUpload(t, home, spec.UploadID, program)
		report, err := runUpgrade(t, home, spec, lockingStub)
		if err != nil || strings.Contains(report, "Stopping peer process") {
			t.Fatalf("an upgrade took the peer %s of another installation for this one's: %v\n%s", otherPID, err, report)
		}
		if exec.Command("kill", "-0", otherPID).Run() != nil {
			t.Fatalf("an upgrade stopped the peer %s of the installation under %s:\n%s", otherPID, other, report)
		}
	}
}

// peerSpelling is a way an owner may start the installed program by hand
// that the program's path does not show: launch is run by bash with HOME
// reaching the installation through a symlink and REAL_HOME the directory
// the symlink resolves to.
type peerSpelling struct {
	name, launch string
}

var peerSpellings = []peerSpelling{
	{"by the resolved path", `exec "$REAL_HOME/.steve-peer/bin/steve" peer --config "$REAL_HOME/.steve-peer/config.json"`},
	{"from the program's directory", `cd "$HOME/.steve-peer/bin" && exec ./steve peer --config "$HOME/.steve-peer/config.json"`},
	{"through PATH", `PATH="$HOME/.steve-peer/bin:$PATH" exec steve peer --config "$HOME/.steve-peer/config.json"`},
}

// symlinkedPeer lays out an installation, without starting its peer, and
// returns the directory holding it and a symlink to that directory, the
// way a home directory can be reached through a symlink.
func symlinkedPeer(t *testing.T) (real, home string) {
	t.Helper()
	real, _ = layoutPeer(t)
	home = filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("pkill", "-KILL", "-f", "^"+home+"/.steve-peer/bin/steve peer ").Run() })
	return real, home
}

// spelledPeer starts the installation's peer as spelling says, holding
// its gateway lock, and returns its pid once the lock records it.
func spelledPeer(t *testing.T, real, home string, spelling peerSpelling) string {
	t.Helper()
	start := exec.Command("bash", "-c", `(`+spelling.launch+`) >> "$HOME/.steve-peer/peer.log" 2>&1 < /dev/null & echo $!`)
	start.Env = append(os.Environ(), "HOME="+home, "REAL_HOME="+real, "STEVE_NODEBOOTSTRAP_STUB=1", lockingStub)
	out, err := start.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{real, home} {
		config := filepath.Join(dir, ".steve-peer", "config.json")
		t.Cleanup(func() { _ = exec.Command("pkill", "-KILL", "-f", "steve peer --config "+config).Run() })
	}
	pid := strings.TrimSpace(string(out))
	awaitGatewayLock(t, real, pid)
	return pid
}

// awaitGatewayLock waits until the gateway lock of the installation under
// home records pid, as it does once that peer holds it.
func awaitGatewayLock(t *testing.T, home, pid string) {
	t.Helper()
	lock := filepath.Join(home, ".steve-peer", "cluster", "peer-process", "gateway.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if recorded, _ := os.ReadFile(lock); strings.TrimSpace(string(recorded)) == pid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the peer %s did not take the gateway lock %s", pid, lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A process holding the installation's gateway lock is the installation's
// peer, whatever its command line says: a peer started by hand by the path
// the installation resolves to, as ./steve or through PATH is found. A
// restart stops it and starts the installed program, and starting a peer
// only when it is down finds it running and leaves it alone.
func TestPeerRestartScriptFindsThePeerHoldingTheGatewayLock(t *testing.T) {
	requirePeerPlatform(t)
	for _, spelling := range peerSpellings {
		t.Run(spelling.name, func(t *testing.T) {
			real, home := symlinkedPeer(t)
			pid := spelledPeer(t, real, home, spelling)
			report, err := runRestart(t, home, RestartSpec{IfStopped: true}, lockingStub)
			if err != nil || !strings.Contains(report, "Peer process "+pid+" is still running") || !strings.Contains(report, "STEVE_RESTART\trunning\n") {
				t.Fatalf("the peer %s holding the gateway lock was not found running: %v\n%s", pid, err, report)
			}
			if exec.Command("kill", "-0", pid).Run() != nil {
				t.Fatalf("the peer %s was stopped where it was to be left alone:\n%s", pid, report)
			}
			report, err = runRestart(t, home, RestartSpec{}, lockingStub)
			if err != nil || !strings.Contains(report, "Stopping peer process "+pid) || !strings.Contains(report, "STEVE_RESTART\trestarted\n") {
				t.Fatalf("the peer %s holding the gateway lock was not restarted: %v\n%s", pid, err, report)
			}
			if exec.Command("kill", "-0", pid).Run() == nil {
				t.Fatalf("the peer %s kept running through the restart:\n%s", pid, report)
			}
			if pids := peerPIDs(t, home); len(pids) != 1 {
				t.Fatalf("expected the installed program to run as the peer, got %v\n%s", pids, report)
			}
		})
	}
}

// Where the gateway lock does not tell who holds it, as lsof may not, the
// command line still does: a peer started by the path the installation
// resolves to is found whether the lock records its pid or not.
func TestPeerRestartScriptFindsAPeerStartedByTheResolvedPathByItsCommandLine(t *testing.T) {
	requirePeerPlatform(t)
	for _, recorded := range []bool{true, false} {
		real, home := symlinkedPeer(t)
		pid := startPeer(t, real)
		if recorded {
			lockedBy(t, real, pid)
		}
		report, err := runRestart(t, home, RestartSpec{IfStopped: true})
		if err != nil || !strings.Contains(report, "Peer process "+pid+" is still running") {
			t.Fatalf("recorded %v: the peer %s started by the resolved path was not found running: %v\n%s", recorded, pid, err, report)
		}
		report, err = runRestart(t, home, RestartSpec{})
		if err != nil || !strings.Contains(report, "Stopping peer process "+pid) || !strings.Contains(report, "STEVE_RESTART\trestarted\n") {
			t.Fatalf("recorded %v: the peer %s started by the resolved path was not restarted: %v\n%s", recorded, pid, err, report)
		}
	}
}

// An upgrade finds the same peer: it is stopped, the program it ran is
// kept as the fallback in place of an earlier one, and the new program
// runs.
func TestPeerUpgradeScriptStopsThePeerHoldingTheGatewayLock(t *testing.T) {
	requirePeerPlatform(t)
	for _, spelling := range peerSpellings {
		t.Run(spelling.name, func(t *testing.T) {
			real, home := symlinkedPeer(t)
			bin := filepath.Join(real, ".steve-peer", "bin")
			running, err := os.ReadFile(filepath.Join(bin, "steve"))
			if err != nil {
				t.Fatal(err)
			}
			earlier := append(slices.Clone(running), "\nearlier program\n"...)
			if err := os.WriteFile(filepath.Join(bin, "steve.previous"), earlier, 0o700); err != nil {
				t.Fatal(err)
			}
			pid := spelledPeer(t, real, home, spelling)
			upgraded := append(slices.Clone(running), "\nnew program\n"...)
			spec := upgradeSpec()
			spec.SHA256 = stageUpload(t, home, spec.UploadID, upgraded)
			report, err := runUpgrade(t, home, spec, lockingStub)
			if err != nil || !strings.Contains(report, "Stopping peer process "+pid) {
				t.Fatalf("the peer %s holding the gateway lock was not stopped for the upgrade: %v\n%s", pid, err, report)
			}
			if kept := programs(t, home); len(kept) != 2 || kept["steve"] != string(upgraded) || kept["steve.previous"] != string(running) {
				t.Fatalf("expected the new program installed and the one that ran kept as the fallback, got %v\n%s", slices.Sorted(maps.Keys(kept)), report)
			}
			if exec.Command("kill", "-0", pid).Run() == nil {
				t.Fatalf("the peer %s kept running through the upgrade:\n%s", pid, report)
			}
			if pids := peerPIDs(t, home); len(pids) != 1 {
				t.Fatalf("expected the new program to run as the peer, got %v\n%s", pids, report)
			}
		})
	}
}

// A search for the peer that cannot be made is not a search that found
// none. Without pgrep or ps, with a pgrep that fails, or with a gateway
// lock it cannot read, the script exits 29 before it stops or starts
// anything: taking the peer for stopped would start a second one beside
// it, or stop one that cannot start again over a lock it cannot open.
func TestPeerRestartScriptFailsWhereItCannotLookForThePeer(t *testing.T) {
	requirePeerPlatform(t)
	failing := t.TempDir()
	if err := os.WriteFile(filepath.Join(failing, "pgrep"), []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, spoil := range map[string]func(t *testing.T, home, pid string) []string{
		"no pgrep": func(t *testing.T, _, _ string) []string { return []string{"PATH=" + pathWithout(t, "pgrep")} },
		"no ps":    func(t *testing.T, _, _ string) []string { return []string{"PATH=" + pathWithout(t, "ps")} },
		"failing pgrep": func(*testing.T, string, string) []string {
			return []string{"PATH=" + failing + ":" + os.Getenv("PATH")}
		},
		"unreadable lock": func(t *testing.T, home, pid string) []string {
			if os.Geteuid() == 0 {
				t.Skip("root reads a lock whatever its mode")
			}
			lockedBy(t, home, pid)
			if err := os.Chmod(filepath.Join(home, ".steve-peer", "cluster", "peer-process", "gateway.lock"), 0); err != nil {
				t.Fatal(err)
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, spec := range []RestartSpec{{IfStopped: true}, {}} {
				home, pid := installedPeer(t)
				report, err := runRestart(t, home, spec, spoil(t, home, pid)...)
				if exitCode(err) != 29 || strings.Contains(report, "STEVE_RESTART") || strings.Contains(report, "Stopping peer process") || !strings.Contains(report, "nothing was stopped or started") {
					t.Fatalf("IfStopped %v: expected exit 29 with nothing stopped or started, got %v\n%s", spec.IfStopped, err, report)
				}
				if pids := peerPIDs(t, home); len(pids) != 1 || pids[0] != pid {
					t.Fatalf("IfStopped %v: the running peer %s was touched: %v\n%s", spec.IfStopped, pid, pids, report)
				}
			}
		})
	}
}

// pathWithout is a PATH with every program the test's PATH finds except
// the one named.
func pathWithout(t *testing.T, program string) string {
	t.Helper()
	dir := t.TempDir()
	for _, from := range filepath.SplitList(os.Getenv("PATH")) {
		entries, _ := os.ReadDir(from)
		for _, entry := range entries {
			if entry.Name() == program || entry.IsDir() {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, entry.Name())); err == nil {
				continue
			}
			if err := os.Symlink(filepath.Join(from, entry.Name()), filepath.Join(dir, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// relativePeer starts the installation's peer from its own directory as
// ./bin/steve, the way its owner may start it by hand, with env added to
// the test's environment, and returns its pid. Its config path names the
// installation, so it is ended by its command line when the test ends,
// never by a pid a later process may have taken.
func relativePeer(t *testing.T, home string, env ...string) string {
	t.Helper()
	start := exec.Command("bash", "-c", `cd "$HOME/.steve-peer"; nohup ./bin/steve peer --config "$HOME/.steve-peer/config.json" >> peer.log 2>&1 < /dev/null & echo $!`)
	start.Env = append(append(os.Environ(), "HOME="+home, "STEVE_NODEBOOTSTRAP_STUB=1"), env...)
	out, err := start.Output()
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, ".steve-peer", "config.json")
	t.Cleanup(func() { _ = exec.Command("pkill", "-KILL", "-f", `^\./bin/steve peer --config `+config).Run() })
	return strings.TrimSpace(string(out))
}

// lockedBy records pid in the installation's gateway lock, as the peer
// holding the lock does.
func lockedBy(t *testing.T, home, pid string) {
	t.Helper()
	dir := filepath.Join(home, ".steve-peer", "cluster", "peer-process")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gateway.lock"), []byte(pid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An upgrade and a restart find and stop the peer with one and the same
// piece of script, so the two cannot drift apart on which process is the
// peer or how long it is given to stop.
func TestPeerUpgradeAndRestartFindAndStopThePeerAlike(t *testing.T) {
	upgrade, err := BuildPeerUpgrade(upgradeSpec())
	if err != nil {
		t.Fatal(err)
	}
	for _, restart := range []string{BuildPeerRestart(RestartSpec{}), BuildPeerRestart(RestartSpec{IfStopped: true})} {
		for name, section := range map[string]string{"start": peerStartSection, "locate": peerLocateSection, "stop": peerStopSection} {
			if strings.TrimSpace(section) == "" || !strings.Contains(upgrade, section) || !strings.Contains(restart, section) {
				t.Fatalf("the %s section is not shared by the upgrade and the restart", name)
			}
		}
		if strings.Contains(restart, "steve.previous") || strings.Contains(restart, "steve.rejected") || strings.Contains(restart, "steve.new") || strings.Contains(restart, "sha256") {
			t.Fatal("a restart must neither verify nor move programs")
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(restart)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("invalid script %s %v", output, err)
		}
	}
}
