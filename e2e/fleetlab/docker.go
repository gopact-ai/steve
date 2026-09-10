package fleetlab

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/text"
)

//go:embed image
var image embed.FS

// nodePort is the port a node listens on inside its own container. What
// varies per node is the published port outside it.
const nodePort = "7701"

// containerHome is the node account's home on a lab machine. It is a
// plain path with nobody's name in it: the suite reads it from the lab
// rather than assuming where a node keeps its work.
const containerHome = "/home/steve"

// docker is a lab whose machines are containers this process started.
type docker struct {
	network    string
	nodes      map[string]Node
	containers map[string]string
	work       string
}

// dockerUnavailable reports why a container lab cannot run here, or "".
func dockerUnavailable() string {
	if _, err := exec.LookPath("docker"); err != nil {
		return "docker is not installed"
	}
	out, err := run(30*time.Second, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "docker is installed but not answering: " + text.FirstLine(strings.TrimSpace(out))
	}
	return ""
}

// startDocker builds the node image if this machine lacks it, then starts
// one container per spec on a private network.
func startDocker(specs []Spec) (backend, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	arch := serverArch()
	work, err := os.MkdirTemp("", "steve-fleetlab-")
	if err != nil {
		return nil, err
	}
	lab := &docker{nodes: map[string]Node{}, containers: map[string]string{}, work: work}
	tag, err := buildImage(work)
	if err != nil {
		lab.close()
		return nil, err
	}
	binaries, err := buildBinaries(root, work, arch)
	if err != nil {
		lab.close()
		return nil, err
	}
	runID := token(6)
	lab.network = "steve-lab-" + runID
	if out, err := run(60*time.Second, "docker", "network", "create", lab.network); err != nil {
		lab.network = ""
		lab.close()
		return nil, fmt.Errorf("create network: %w\n%s", err, out)
	}
	// Both this process and the other containers reach a node through the
	// network's gateway, so the address the hub configures is the same one
	// a peer node dials for a direct artifact transfer.
	gateway, err := networkGateway(lab.network)
	if err != nil {
		lab.close()
		return nil, err
	}
	for _, spec := range specs {
		if err := lab.startNode(spec, runID, tag, gateway, binaries); err != nil {
			lab.close()
			return nil, err
		}
	}
	return lab, nil
}

// startNode runs one machine: a container that outlives its node process,
// the binaries this suite just built, a config, and a started node.
func (d *docker) startNode(spec Spec, runID, tag, gateway string, binaries map[string]string) error {
	name := "steve-lab-" + runID + "-" + spec.Name
	out, err := run(120*time.Second, "docker", "run", "--detach", "--name", name,
		"--network", d.network, "--hostname", spec.Name,
		"--publish", "0.0.0.0::"+nodePort, tag)
	if err != nil {
		return fmt.Errorf("start %s: %w\n%s", spec.Name, err, out)
	}
	d.containers[spec.Name] = name
	published, err := publishedPort(name)
	if err != nil {
		return err
	}
	node := Node{
		Name:  spec.Name,
		Addr:  net.JoinHostPort(gateway, published),
		Token: "lab-" + token(8),
		Home:  containerHome,
		Work:  containerHome + "/steve-work",
	}
	config, err := d.writeConfig(spec, node)
	if err != nil {
		return err
	}
	for _, binary := range []string{"steve-node", "mockagent"} {
		if out, err := run(120*time.Second, "docker", "cp", binaries[binary], name+":"+containerHome+"/steve-bin/"+binary); err != nil {
			return fmt.Errorf("copy %s to %s: %w\n%s", binary, spec.Name, err, out)
		}
	}
	if out, err := run(60*time.Second, "docker", "cp", config, name+":"+containerHome+"/node.json"); err != nil {
		return fmt.Errorf("copy config to %s: %w\n%s", spec.Name, err, out)
	}
	d.nodes[spec.Name] = node
	if err := d.start(spec.Name); err != nil {
		return err
	}
	if err := waitDialable(node.Addr, 60*time.Second); err != nil {
		log, _ := d.exec(spec.Name, "tail -n 40 "+containerHome+"/steve-node.log")
		return fmt.Errorf("%s never answered: %w\n%s", spec.Name, err, log)
	}
	return nil
}

// writeConfig renders the node's own configuration file. The mock harness
// is the acceptance agent every scenario that does not need a real model
// runs on; a real-model run supplies its machines through Remote instead.
func (d *docker) writeConfig(spec Spec, node Node) (string, error) {
	config := map[string]any{
		"name":   spec.Name,
		"listen": ":" + nodePort,
		"token":  node.Token,
		"harnesses": map[string]any{
			"mock": map[string]any{"command": containerHome + "/steve-bin/mockagent"},
		},
		// A node reports these to the hub, which builds every path it
		// later asks the node for from them: without a workspace root the
		// hub would ask for a checkout at a relative path and the node
		// would refuse it.
		"workspace_root": node.Work,
		"state_dir":      containerHome + "/steve-state",
	}
	if len(spec.Declares) > 0 {
		config["declares"] = spec.Declares
	}
	if len(spec.Capabilities) > 0 {
		config["capabilities"] = spec.Capabilities
	}
	if len(spec.Tools) > 0 {
		config["tools"] = spec.Tools
	}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(d.work, spec.Name+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (d *docker) node(name string) (Node, bool) { n, ok := d.nodes[name]; return n, ok }

func (d *docker) exec(name, script string) (string, error) {
	return run(2*time.Minute, "docker", "exec", d.containers[name], "bash", "-lc", script)
}

func (d *docker) stop(name string) error {
	out, err := d.exec(name, "nodectl stop")
	if err != nil {
		return fmt.Errorf("stop %s: %w\n%s", name, err, out)
	}
	return nil
}

func (d *docker) start(name string) error {
	out, err := d.exec(name, "nodectl start")
	if err != nil {
		return fmt.Errorf("start %s: %w\n%s", name, err, out)
	}
	return nil
}

// close removes what this lab created. Every step is attempted even when
// an earlier one fails: a leaked container costs the next run its name.
func (d *docker) close() {
	for _, container := range d.containers {
		_, _ = run(60*time.Second, "docker", "rm", "--force", "--volumes", container)
	}
	if d.network != "" {
		_, _ = run(60*time.Second, "docker", "network", "rm", d.network)
	}
	if d.work != "" {
		_ = os.RemoveAll(d.work)
	}
}

// buildImage builds the node image unless this machine already has it.
// The tag carries a digest of the image's own sources, so an edit to the
// Dockerfile or nodectl produces a new tag instead of a stale hit.
func buildImage(work string) (string, error) {
	files, err := imageFiles()
	if err != nil {
		return "", err
	}
	tag := "steve-fleetlab-node:" + imageDigest(files)
	if _, err := run(30*time.Second, "docker", "image", "inspect", tag); err == nil {
		return tag, nil
	}
	context := filepath.Join(work, "image")
	if err := os.MkdirAll(context, 0o755); err != nil {
		return "", err
	}
	for name, content := range files {
		mode := os.FileMode(0o644)
		if name == "nodectl" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(context, name), content, mode); err != nil {
			return "", err
		}
	}
	// The first build on a machine installs packages, which is slower than
	// anything else the lab does and happens once.
	if out, err := run(10*time.Minute, "docker", "build", "--tag", tag, context); err != nil {
		return "", fmt.Errorf("build the node image: %w\n%s", err, out)
	}
	return tag, nil
}

func imageFiles() (map[string][]byte, error) {
	entries, err := image.ReadDir("image")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		content, err := image.ReadFile("image/" + entry.Name())
		if err != nil {
			return nil, err
		}
		files[entry.Name()] = content
	}
	return files, nil
}

func imageDigest(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	sum := sha256.New()
	for _, name := range names {
		sum.Write([]byte(name))
		sum.Write(files[name])
	}
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

// buildBinaries compiles what a node runs, for the architecture the
// Docker server runs on rather than this process's own.
func buildBinaries(root, work, arch string) (map[string]string, error) {
	built := map[string]string{}
	for _, target := range []struct{ name, pkg string }{
		{"steve-node", "./cmd/steve-node"},
		{"mockagent", "./cmd/mockagent"},
	} {
		path := filepath.Join(work, target.name)
		cmd := exec.Command("go", "build", "-o", path, target.pkg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build %s: %w\n%s", target.name, err, out)
		}
		built[target.name] = path
	}
	return built, nil
}

// moduleRoot is the repository the suite is running from, found through
// the go tool so it does not depend on where a test's working directory
// happens to be.
func moduleRoot() (string, error) {
	out, err := run(30*time.Second, "go", "env", "GOMOD")
	if err != nil {
		return "", fmt.Errorf("locate the module: %w\n%s", err, out)
	}
	gomod := strings.TrimSpace(out)
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("no go.mod above the working directory")
	}
	return filepath.Dir(gomod), nil
}

// serverArch is the architecture of the machine running the containers,
// which is not this process's when Docker is remote.
func serverArch() string {
	out, err := run(30*time.Second, "docker", "version", "--format", "{{.Server.Arch}}")
	if arch := strings.TrimSpace(out); err == nil && arch != "" {
		return arch
	}
	return "amd64"
}

func networkGateway(network string) (string, error) {
	out, err := run(30*time.Second, "docker", "network", "inspect", network,
		"--format", "{{(index .IPAM.Config 0).Gateway}}")
	gateway := strings.TrimSpace(out)
	if err != nil || gateway == "" {
		return "", fmt.Errorf("read the network's gateway: %w\n%s", err, out)
	}
	return gateway, nil
}

// publishedPort is the port on the lab's host that reaches a container's
// node. Docker prints one line per protocol family; the first with a port
// is the one to dial.
func publishedPort(container string) (string, error) {
	out, err := run(30*time.Second, "docker", "port", container, nodePort+"/tcp")
	if err != nil {
		return "", fmt.Errorf("read the published port of %s: %w\n%s", container, err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if _, port, err := net.SplitHostPort(strings.TrimSpace(line)); err == nil && port != "" {
			return port, nil
		}
	}
	return "", fmt.Errorf("%s published no port for %s", container, nodePort)
}

func waitDialable(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		if last = dialable(addr); last == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

func dialable(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// run executes a command and returns its combined output, killing it if
// it outlasts the deadline: a docker call that hangs must not hang the
// suite with it.
func run(within time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("%s timed out after %s", name, within)
	}
	return string(out), err
}

// token is a name or secret nothing else in this run will collide with.
func token(bytes int) string {
	raw := make([]byte, bytes)
	// crypto/rand.Read never returns an error; it aborts the program
	// instead when the platform cannot supply randomness.
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
