// Package fleetlab gives an acceptance suite the machines it needs.
//
// The three-host scenarios are about what happens between separate
// machines: a process that must run over there and not here, a node that
// goes away mid-step, an artifact that travels from one node to another
// without the hub in the data path. None of that can be faked in one
// process, but none of it needs hardware either — a container per node
// gives each one its own filesystem, process table and address.
//
// A lab hands out three things per node: an address the hub dials, a way
// to run a command on that machine, and a way to stop and start its node
// process. Scenarios use those instead of naming a host, so the suite
// carries no addresses of its own and runs wherever Docker does.
//
// Pointing the same scenarios at real machines stays possible, for the
// runs that need real agents and real credentials: set the environment
// variables in Remote for every node and the lab attaches to those
// instead of starting containers.
package fleetlab

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

// Spec is a machine the suite wants. Declares are the capabilities nobody
// can verify from inside a process ("gpu", "network:internal"), which a
// node offers on the operator's word.
type Spec struct {
	Name         string
	Declares     []string
	Capabilities []string
	Tools        []string
	// SessionGrace is how long this node's sessions survive a lost hub
	// connection, waiting to be resumed. Zero leaves the node's own
	// default (ten minutes), which is longer than a scenario about losing
	// a node can wait for.
	//
	// It applies to machines the lab starts. A machine the operator
	// supplies keeps whatever grace it was started with — the lab cannot
	// change a setting a node reads at startup without restarting it, and
	// that node is not the lab's to restart. A scenario that needs the
	// real figure reads it from the node's advert, which carries it.
	SessionGrace time.Duration
}

// Node is one machine of a running lab.
type Node struct {
	// Name is what the hub calls this node.
	Name string
	// Addr is host:port for the node's wire protocol. It resolves both
	// from the process running the suite and from the other nodes, so an
	// artifact can travel node to node under the address the hub knows.
	Addr string
	// Token is what a hub presents to be admitted here.
	Token string
	// Home is the node account's home directory on that machine.
	Home string
	// Work is the directory a node's sessions and worktrees live under.
	Work string
}

// A backend is what a lab actually runs on.
type backend interface {
	node(name string) (Node, bool)
	exec(name, script string) (string, error)
	stop(name string) error
	start(name string) error
	close()
}

// Lab is a running set of machines.
type Lab struct {
	back  backend
	names []string
}

// ErrUnavailable means no container runtime is available. Invalid lab
// configuration and failures while starting machines are ordinary errors.
var ErrUnavailable = errors.New("fleetlab unavailable")

// Unavailable reports why a lab cannot run here, or "" when one can. A
// suite calls it before Open so it can skip with a reason rather than
// fail as if the code under test were broken.
func Unavailable() string { return dockerUnavailable() }

// Open brings the machines up. The caller closes the lab; a suite that
// shares one across its scenarios does that from TestMain.
//
// Machines named in the environment are used as they are, which is how a
// run that needs real agents and credentials points the same scenarios at
// real hardware. Otherwise Open starts a container per node.
func Open(specs ...Spec) (*Lab, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("fleetlab: a lab needs at least one node")
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		if strings.TrimSpace(spec.Name) == "" {
			return nil, fmt.Errorf("fleetlab: every node needs a name")
		}
		names = append(names, spec.Name)
	}
	back, err := attachRemote(specs)
	if err != nil {
		return nil, fmt.Errorf("fleetlab: %w", err)
	}
	if back == nil {
		if reason := dockerUnavailable(); reason != "" {
			return nil, fmt.Errorf("%w: %s; set the %s variables to use machines you already have",
				ErrUnavailable, reason, remoteEnvExample(specs[0].Name))
		}
		if back, err = startDocker(specs); err != nil {
			return nil, fmt.Errorf("fleetlab: %w", err)
		}
	}
	return &Lab{back: back, names: names}, nil
}

// Start is Open for a scenario that wants a lab to itself, tied to the
// test's lifetime. It skips when there is nothing to run machines on.
func Start(t testing.TB, specs ...Spec) *Lab {
	t.Helper()
	lab, err := Open(specs...)
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("%v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lab.Close)
	return lab
}

// Node reports one machine. An unknown name is a mistake in the suite
// rather than a condition to handle, so it stops the run where it is.
func (l *Lab) Node(name string) Node {
	n, ok := l.back.node(name)
	if !ok {
		panic(fmt.Sprintf("fleetlab: no node %q in this lab (have %s)", name, strings.Join(l.names, ", ")))
	}
	return n
}

// Addr is the node's wire address.
func (l *Lab) Addr(name string) string { return l.Node(name).Addr }

// Token is what the hub presents to that node.
func (l *Lab) Token(name string) string { return l.Node(name).Token }

// Work is the directory that node's sessions run under.
func (l *Lab) Work(name string) string { return l.Node(name).Work }

// Names lists the machines, in the order they were asked for.
func (l *Lab) Names() []string { return append([]string(nil), l.names...) }

// Exec runs a shell command on the node's own machine and returns its
// combined output. This is how a scenario proves where something
// happened: the marker file, the process, the log line are all read from
// the machine that was supposed to do the work.
func (l *Lab) Exec(name, script string) (string, error) {
	l.Node(name)
	return l.back.exec(name, script)
}

// MustExec is Exec for a command whose failure leaves the scenario with
// nothing to assert. The test is passed per call so one lab can be shared
// by a whole suite.
func (l *Lab) MustExec(t testing.TB, name, script string) string {
	t.Helper()
	out, err := l.Exec(name, script)
	if err != nil {
		t.Fatalf("on %s: %s: %v\n%s", name, script, err, out)
	}
	return out
}

// StopNode ends the node process and leaves the machine up, the way an
// operator stopping a node does. Its work and logs survive.
func (l *Lab) StopNode(name string) error {
	l.Node(name)
	return l.back.stop(name)
}

// StartNode brings a stopped node back, and waits for it to answer:
// a restart still in progress looks exactly like a node that stayed
// down, and whatever ran next would fail for the wrong reason.
func (l *Lab) StartNode(name string) error {
	node := l.Node(name)
	if err := l.back.start(name); err != nil {
		return err
	}
	if err := waitDialable(node.Addr, 30*time.Second); err != nil {
		return fmt.Errorf("node %s did not answer on %s: %w", name, node.Addr, err)
	}
	return nil
}

// Close releases whatever the lab started. Machines the operator gave it
// are left alone.
func (l *Lab) Close() { l.back.close() }

// remoteEnv is the variable naming for a node the operator supplies:
// STEVE_LAB_NODE_A_ADDR and friends, for a node called node-a.
func remoteEnv(name, field string) string {
	return "STEVE_LAB_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name)) + "_" + field
}

func remoteEnvExample(name string) string {
	return remoteEnv(name, "ADDR") + " / " + remoteEnv(name, "TOKEN")
}

// remote is a lab over machines that already exist: addresses and tokens
// come from the environment, commands go over ssh, and the node process is
// managed by the operator's own script.
type remote struct {
	nodes map[string]Node
	ctl   string
}

// attachRemote returns a lab over supplied machines, or nil when the
// environment names none. Naming some but not all is a mistake worth
// reporting: half a fleet is not a fleet.
func attachRemote(specs []Spec) (backend, error) {
	nodes := map[string]Node{}
	var missing []string
	for _, spec := range specs {
		addr := strings.TrimSpace(os.Getenv(remoteEnv(spec.Name, "ADDR")))
		if addr == "" {
			missing = append(missing, spec.Name)
			continue
		}
		home := strings.TrimSpace(os.Getenv(remoteEnv(spec.Name, "HOME")))
		if !path.IsAbs(home) {
			return nil, fmt.Errorf("%s must be an absolute path", remoteEnv(spec.Name, "HOME"))
		}
		token := strings.TrimSpace(os.Getenv(remoteEnv(spec.Name, "TOKEN")))
		if token == "" {
			return nil, fmt.Errorf("%s must not be empty", remoteEnv(spec.Name, "TOKEN"))
		}
		nodes[spec.Name] = Node{
			Name:  spec.Name,
			Addr:  addr,
			Token: token,
			Home:  home,
			Work:  home + "/steve-work",
		}
	}
	switch {
	case len(nodes) == 0:
		return nil, nil
	case len(missing) > 0:
		return nil, fmt.Errorf("%s has no address; supply every node or none", strings.Join(missing, " and "))
	}
	ctl := strings.TrimSpace(os.Getenv("STEVE_LAB_NODECTL"))
	if ctl == "" {
		ctl = "~/steve-bin/nodectl"
	}
	return &remote{nodes: nodes, ctl: ctl}, nil
}

func (r *remote) node(name string) (Node, bool) { n, ok := r.nodes[name]; return n, ok }

func (r *remote) exec(name, script string) (string, error) {
	host := hostOf(r.nodes[name].Addr)
	return run(2*time.Minute, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=10", "-n", host, script)
}

func (r *remote) stop(name string) error {
	out, err := r.exec(name, r.ctl+" stop")
	if err != nil {
		return fmt.Errorf("stop %s: %w\n%s", name, err, out)
	}
	return nil
}

func (r *remote) start(name string) error {
	out, err := r.exec(name, r.ctl+" start")
	if err != nil {
		return fmt.Errorf("start %s: %w\n%s", name, err, out)
	}
	return nil
}

// close leaves the operator's machines exactly as they were found.
func (r *remote) close() {}

// hostOf is the host half of host:port, for ssh.
func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
