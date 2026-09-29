//go:build linux

package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/filedoc"
)

// orphaningShell starts the agent the way a harness can: through a shell
// that leaves a member of the agent's process group running on its own. The
// member's argument is unique to one test, so cleanup never signals a
// process the test did not start.
const orphaningShell = `echo $$ > "$2/leader"; sleep "$3" </dev/null >/dev/null 2>&1 & echo $! > "$2/member"; exec "$1"`

func orphaningNodeConfig(dir, agent, pause string) ServerConfig {
	return ServerConfig{Name: "worker", StateDir: filepath.Join(dir, "state"), WorkspaceRoot: filepath.Join(dir, "workspace"),
		Harnesses:         map[string]HarnessSpec{"orphaning": {Command: "/bin/sh", Args: []string{"-c", orphaningShell, "orphaning", agent, dir, pause}}},
		SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}}
}

// TestNodeSessionKilledNodeSubprocess is the node a restart test kills: it
// opens one native session and waits, the session's agent running, to be
// killed.
func TestNodeSessionKilledNodeSubprocess(t *testing.T) {
	dir := os.Getenv("STEVE_KILLED_NODE_DIR")
	if dir == "" {
		return
	}
	s := NewServer(orphaningNodeConfig(dir, os.Getenv("STEVE_KILLED_NODE_AGENT"), os.Getenv("STEVE_KILLED_NODE_PAUSE")))
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	req := nodeSessionRequest("open")
	req.Harness, req.Workdir, req.CommandID = "orphaning", filepath.Join(dir, "workspace"), "open-orphaning"
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&filedoc.Document{Path: filepath.Join(dir, "session")}).Save([]byte(state.ID)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

// killedNode is a node that was killed with a session's agent running. Its
// configuration restarts it on the same state.
type killedNode struct {
	cfg               ServerConfig
	id                string
	dir, agent, pause string
	leader, member    int
}

// end kills what the agent's shell recorded, each only while it is still
// the process the test started.
func (k killedNode) end() {
	if pid := recordedPID(filepath.Join(k.dir, "member")); pid > 0 && cmdlineIs(pid, "sleep", k.pause) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if pid := recordedPID(filepath.Join(k.dir, "leader")); pid > 0 && cmdlineIs(pid, k.agent) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// killNode runs a node in a process of its own, opens a session there whose
// agent leaves a member in its process group, and kills the node before it
// can stop anything.
func killNode(t *testing.T) killedNode {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	agent := buildMockAgent(t)
	pause := fmt.Sprintf("%d.%06d", 3000+os.Getpid()%997, time.Now().Nanosecond()/1000)
	killed := killedNode{cfg: orphaningNodeConfig(dir, agent, pause), dir: dir, agent: agent, pause: pause}
	t.Cleanup(killed.end)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	node := exec.Command(executable, "-test.run=^TestNodeSessionKilledNodeSubprocess$", "-test.timeout=90s")
	node.Env = append(os.Environ(), "STEVE_KILLED_NODE_DIR="+dir, "STEVE_KILLED_NODE_AGENT="+agent, "STEVE_KILLED_NODE_PAUSE="+pause)
	log, err := os.Create(filepath.Join(dir, "node.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	node.Stdout, node.Stderr = log, log
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = node.Process.Kill()
		_ = node.Wait()
	}()
	waitSessionTestFile(t, filepath.Join(dir, "session"))
	if err := node.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = node.Wait()
	raw, err := os.ReadFile(filepath.Join(dir, "session"))
	if err != nil {
		t.Fatal(err)
	}
	killed.id = string(raw)
	killed.leader, killed.member = recordedPID(filepath.Join(dir, "leader")), recordedPID(filepath.Join(dir, "member"))
	return killed
}

// A killed node leaves its agent's process group running and no one to stop
// it. The restarted node must end that group and confirm the stop, or the
// hub can never finish stopping the execution.
func TestNodeSessionRestartEndsAKilledNodesProcessGroup(t *testing.T) {
	killed := killNode(t)
	if !liveProcess(killed.member) {
		t.Fatal("the group member ended with the node; the restart has nothing to stop")
	}
	restarted := NewServer(killed.cfg)
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	req := nodeSessionRequest("abort")
	req.ID = killed.id
	state, err := restarted.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || !state.ProcessStopped {
		t.Fatalf("restarted node did not confirm the stop: state=%s process_stopped=%v: %v", state.State, state.ProcessStopped, err)
	}
	if liveProcess(killed.member) {
		t.Fatal("a stop was confirmed while a member of the agent's process group still ran")
	}
}

// The agents a killed node left can no longer be reached, and until they end
// the restarted node cannot show that its processes stopped. It ends them as
// it loads the records, before anyone asks for a stop.
func TestNodeSessionRestartEndsAKilledNodesProcessGroupOnLoad(t *testing.T) {
	killed := killNode(t)
	if !liveProcess(killed.member) {
		t.Fatal("the group member ended with the node; the restart has nothing to stop")
	}
	restarted := NewServer(killed.cfg)
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	if liveProcess(killed.member) {
		t.Fatal("the restarted node left the killed node's agent process group running until a stop was asked for")
	}
	if !restarted.sessions.processesStopped() {
		t.Fatal("the restarted node did not confirm that the killed node's processes stopped")
	}
}

// Pids are handed out again while a node is down. A process that now has
// the recorded leader's pid and leads a group of that id, but started at
// another time, is not the agent, and the restarted node must not signal it.
// Its pid being given out again proves the recorded group had emptied, so
// the stop is confirmed.
func TestNodeSessionRestartLeavesAReusedProcessGroupAlone(t *testing.T) {
	killed := killNode(t)
	// The test ends the recorded group itself, as if it had ended while the
	// node was down.
	killed.end()
	for deadline := time.Now().Add(10 * time.Second); liveProcess(killed.member) || liveProcess(killed.leader); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the recorded group did not end")
		}
	}
	reuse := exec.Command("sleep", fmt.Sprintf("%d.%06d", 4000+os.Getpid()%997, time.Now().Nanosecond()/1000))
	reuse.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := reuse.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		_ = reuse.Wait()
		close(ended)
	}()
	defer func() {
		_ = reuse.Process.Kill()
		<-ended
	}()
	recordProcessGroup(t, killed.cfg, killed.id, reuse.Process.Pid)
	restarted := NewServer(killed.cfg)
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	req := nodeSessionRequest("abort")
	req.ID = killed.id
	state, err := restarted.sessions.Do(t.Context(), "cluster-1", req)
	select {
	case <-ended:
		t.Fatal("the restarted node signalled a process that only reused the recorded pid")
	default:
	}
	if err != nil || !state.ProcessStopped {
		t.Fatalf("a reused pid did not confirm that the recorded group ended: state=%s process_stopped=%v: %v", state.State, state.ProcessStopped, err)
	}
}

// When the recorded leader is gone and no member left in its group carries
// the recorded mark, a restarted node cannot show the group is the
// execution's. It signals nothing and the stop stays unconfirmed; once the
// members are ended by hand, the next stop checks again and confirms it.
func TestNodeSessionRestartLeavesAnUnprovenGroupUntilItEnds(t *testing.T) {
	killed := killNode(t)
	rewriteRecordedProcess(t, killed.cfg, killed.id, map[string]json.RawMessage{"mark": json.RawMessage(`"another-group"`)})
	if cmdlineIs(killed.leader, killed.agent) {
		_ = syscall.Kill(killed.leader, syscall.SIGKILL)
	}
	for deadline := time.Now().Add(10 * time.Second); processExists(killed.leader); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the recorded leader was not reaped")
		}
	}
	if !liveProcess(killed.member) {
		t.Fatal("the group member ended with its leader; nothing is left to prove")
	}
	restarted := NewServer(killed.cfg)
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	req := nodeSessionRequest("abort")
	req.ID = killed.id
	state, err := restarted.sessions.Do(t.Context(), "cluster-1", req)
	if err == nil || state.ProcessStopped {
		t.Fatalf("a stop was confirmed for a group the node could not show was the execution's: state=%s process_stopped=%v", state.State, state.ProcessStopped)
	}
	if !liveProcess(killed.member) {
		t.Fatal("the restarted node signalled a group it could not show was the execution's")
	}
	killed.end()
	for deadline := time.Now().Add(10 * time.Second); liveProcess(killed.member); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the group member did not end")
		}
	}
	state, err = restarted.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || !state.ProcessStopped {
		t.Fatalf("the stop was not confirmed after the group ended: state=%s process_stopped=%v: %v", state.State, state.ProcessStopped, err)
	}
}

// recordProcessGroup points a session record's process identity at pid,
// leaving the recorded start time as it was.
func recordProcessGroup(t *testing.T, cfg ServerConfig, id string, pid int) {
	t.Helper()
	rewriteRecordedProcess(t, cfg, id, map[string]json.RawMessage{"leader": json.RawMessage(strconv.Itoa(pid)), "group": json.RawMessage(strconv.Itoa(pid))})
}

// rewriteRecordedProcess replaces fields of a session record's process
// identity.
func rewriteRecordedProcess(t *testing.T, cfg ServerConfig, id string, fields map[string]json.RawMessage) {
	t.Helper()
	store, err := openSessionRecords(filepath.Join(cfg.StateDir, "node-sessions", "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	var header []byte
	if err := store.db.QueryRow(`SELECT header FROM sessions WHERE id=?`, id).Scan(&header); err != nil {
		t.Fatal(err)
	}
	var record, process map[string]json.RawMessage
	if err := json.Unmarshal(header, &record); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(record["process"], &process); err != nil || process["leader"] == nil || process["group"] == nil || process["start"] == nil {
		t.Fatal("session record keeps no process identity")
	}
	maps.Copy(process, fields)
	raw, err := json.Marshal(process)
	if err != nil {
		t.Fatal(err)
	}
	record["process"] = raw
	if header, err = json.Marshal(record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET header=? WHERE id=?`, header, id); err != nil {
		t.Fatal(err)
	}
}

func recordedPID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

func cmdlineIs(pid int, argv ...string) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.Equal(raw, []byte(strings.Join(argv, "\x00")+"\x00"))
}

// processExists is true while anything holds pid, a zombie included.
func processExists(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// liveProcess is false for a pid that is gone or a zombie: neither runs.
func liveProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(raw, ')')
	fields := strings.Fields(string(raw[end+1:]))
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}
