package sshconnect

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Linker is implemented by a backend whose node the coordinator reaches
// through the enrolling SSH session rather than the network: the session
// carries the cluster protocol both ways, so neither machine needs a route
// to the other. Link opens that session, which runs the installed program
// on the machine as its far end, and returns once it is up.
type Linker interface {
	Link(ctx context.Context, installID string, registration Registration) error
}

// The link binds this machine's listeners on the target's loopback at one
// of these ports. They sit below every ephemeral range (Linux hands out
// 32768 and up, macOS 49152 and up), so an outgoing connection on the
// target cannot take a port while the session is down; the target's own
// node ports are excluded by the backend when it picks.
const (
	FirstLoopbackPort = 25407
	LastLoopbackPort  = 25426
)

var loopbackPortCandidates = func() []int {
	ports := make([]int, 0, LastLoopbackPort-FirstLoopbackPort+1)
	for port := FirstLoopbackPort; port <= LastLoopbackPort; port++ {
		ports = append(ports, port)
	}
	return ports
}()

const loopbackProbeTimeout = 10 * time.Second

// loopbackProbeScript tries to open each candidate port on the target's
// loopback. A refused connection means the port is free; anything that
// answers, including a listener on all interfaces, means it is not. The
// final "end" line proves the script ran to completion.
func loopbackProbeScript(ports []int) string {
	var b strings.Builder
	b.WriteString("for port in")
	for _, port := range ports {
		b.WriteString(" " + strconv.Itoa(port))
	}
	b.WriteString(loopbackProbeBody)
	return b.String()
}

const loopbackProbeBody = `; do
  if ( exec 3<>"/dev/tcp/127.0.0.1/$port" ) 2>/dev/null; then
    printf 'STEVE_PORT\t%s\tbusy\n' "$port"
  else
    printf 'STEVE_PORT\t%s\tfree\n' "$port"
  fi
done
printf 'STEVE_PORT\tend\tfree\n'
`

// parseLoopbackProbe reads the free ports the target reported, in order.
// Without the end line the script was cut off and nothing is known.
func parseLoopbackProbe(stdout string, candidates []int) ([]int, bool) {
	free, ended := map[int]bool{}, false
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 3 || fields[0] != "STEVE_PORT" {
			continue
		}
		if fields[1] == "end" {
			ended = true
			continue
		}
		if port, err := strconv.Atoi(fields[1]); err == nil && fields[2] == "free" {
			free[port] = true
		}
	}
	if !ended {
		return nil, false
	}
	out := make([]int, 0, len(candidates))
	for _, port := range candidates {
		if free[port] {
			out = append(out, port)
		}
	}
	sort.Ints(out)
	return out, true
}

// probeLoopbackPorts records, on the check, which loopback ports on the
// target a link could bind. A backend that does not link, a target without
// bash, or a probe that did not finish leaves the check silent on the
// matter rather than wrong.
func (s *Service) probeLoopbackPorts(ctx context.Context, connection Connection, check *CheckResult) {
	if _, ok := s.backend.(Linker); !ok || !check.HasTool("bash") {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, loopbackProbeTimeout)
	defer cancel()
	output, err := connection.Run(probeCtx, "bash -s", loopbackProbeScript(loopbackPortCandidates))
	if err != nil {
		return
	}
	if free, ok := parseLoopbackProbe(output.Stdout, loopbackPortCandidates); ok {
		check.FreeLoopbackPorts = free
	}
}
