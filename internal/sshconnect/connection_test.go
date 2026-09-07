package sshconnect

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func bindingFixture(t *testing.T) (OpenSSH, string, string) {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	log := filepath.Join(dir, "connections")
	if err := os.WriteFile(config, []byte("HostName original.example\nUser original-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
prev=''; control=''; config=''; action=''; master=0; proxy=''
for value do
  case "$prev" in -S) control="$value";; -F) config="$value";; -O) action="$value";; -o) case "$value" in ProxyCommand=*) proxy="$value";; esac;; esac
  [ "$value" != '-M' ] || master=1
  prev="$value"
done
if [ "$master" = 1 ]; then
  awk '/HostName/{host=$2}/User/{user=$2}END{print host "/" user}' "$config" > "$control.target"
  exit 0
fi
case "$action" in
 check) [ -f "$control.target" ]; exit $?;;
 exit) rm -f "$control.target"; exit 0;;
esac
[ "$config" = '/dev/null' ] || exit 81
[ "$proxy" = 'ProxyCommand=/usr/bin/false' ] || exit 82
[ -f "$control.target" ] || exit 83
cat "$control.target" >> '` + log + `'
cat > '` + log + `.input'
`
	binary := filepath.Join(dir, "ssh-fixture")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return OpenSSH{Binary: binary}, config, log
}

func TestOpenSSHConnectionKeepsHostAndUserWhenConfigChanges(t *testing.T) {
	runner, config, log := bindingFixture(t)
	connection, err := runner.Bind(t.Context(), "dev", []string{"-o", "ControlMaster=no", "-o", "ControlPath=none", "-F", config, "--", "dev", ""})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Run(t.Context(), "sh -s", "read-only check"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("HostName different.example\nUser another-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Upload(t.Context(), "receive-binary", strings.NewReader("binary")); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Run(t.Context(), "bash -s", "private enrollment package"); err != nil {
		t.Fatal(err)
	}
	connections, err := os.ReadFile(log)
	if err != nil || string(connections) != strings.Repeat("original.example/original-user\n", 3) {
		t.Fatalf("config redirected an authenticated connection: %q %v", connections, err)
	}
}

func TestOpenSSHConnectionLossCannotRedirectPrivateInput(t *testing.T) {
	runner, config, log := bindingFixture(t)
	connection, err := runner.Bind(t.Context(), "dev", []string{"-F", config, "--", "dev", ""})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	bound := connection.(*sshConnection)
	if err := os.Remove(bound.socket + ".target"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Run(t.Context(), "bash -s", "private-package"); err == nil {
		t.Fatal("lost connection silently reconnected")
	}
	if _, err := os.Stat(log + ".input"); !os.IsNotExist(err) {
		t.Fatal("private input reached a replacement connection")
	}
}

func TestRealOpenSSHMissingMasterFailsWithoutReadingConfigOrConnecting(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH not available")
	}
	dir := t.TempDir()
	connection := &sshConnection{runner: OpenSSH{}, dir: dir, socket: filepath.Join(dir, "missing")}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	output, err := connection.Run(ctx, "bash -s", "private-sentinel")
	if err == nil || strings.Contains(output.Stdout+output.Stderr, "private-sentinel") {
		t.Fatalf("missing master leaked input or succeeded: %#v %v", output, err)
	}
}

type trackedConnection struct {
	*fixtureConnection
	closed chan struct{}
	once   sync.Once
}

func (c *trackedConnection) Close() error { c.once.Do(func() { close(c.closed) }); return nil }

type trackedBinder struct {
	*recordingRunner
	connections []*trackedConnection
}

func (r *trackedBinder) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	c := &trackedConnection{fixtureConnection: &fixtureConnection{runner: r.recordingRunner, args: append([]string{}, args...)}, closed: make(chan struct{})}
	r.connections = append(r.connections, c)
	return c, nil
}
func (r *trackedBinder) Upload(ctx context.Context, args []string, input io.Reader) (Output, error) {
	return r.recordingRunner.Upload(ctx, args, input)
}

func TestPlanUsesOneBoundConnectionUntilCompletion(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	binder := &trackedBinder{recordingRunner: r}
	svc.runner = binder
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(binder.connections) != 1 {
		t.Fatal("plan did not establish exactly one connection")
	}
	select {
	case <-binder.connections[0].closed:
		t.Fatal("plan closed connection before approval")
	default:
	}
	if _, err := svc.Commit(t.Context(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if len(binder.connections) != 1 {
		t.Fatal("commit reconnected instead of reusing checked connection")
	}
	select {
	case <-binder.connections[0].closed:
	default:
		t.Fatal("completion leaked control master")
	}
}

func TestExpiredPlanClosesItsBoundConnection(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	binder := &trackedBinder{recordingRunner: r}
	svc.runner = binder
	svc.ttl = 20 * time.Millisecond
	if _, err := svc.Plan(t.Context(), installRequest()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-binder.connections[0].closed:
	case <-time.After(time.Second):
		t.Fatal("expired plan leaked control master")
	}
}

func TestServiceCloseDropsPendingPlanConnectionAndRejectsNewBind(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	binder := &trackedBinder{recordingRunner: r}
	svc.runner = binder
	plan, err := svc.Plan(t.Context(), installRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-binder.connections[0].closed:
	default:
		t.Fatal("shutdown retained private master")
	}
	if _, err := svc.Check(t.Context(), "dev"); err == nil {
		t.Fatal("closed service reconnected")
	}
	if _, err := svc.Commit(t.Context(), plan.ID); err == nil {
		t.Fatal("closed service resumed deployment")
	}
	if len(binder.connections) != 1 {
		t.Fatal("closed service created another connection")
	}
}
