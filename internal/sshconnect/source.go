package sshconnect

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// SourceHost is one of this machine's addresses, and whether the target
// machine could open a TCP connection to it during the check.
type SourceHost struct {
	Host      string `json:"host"`
	Reachable bool   `json:"reachable"`
}

// SourceAdvisor is implemented by a backend whose installed node must
// connect back to this machine. It names the addresses worth trying and
// the port the target should be able to open; the check asks the target
// itself, since only the target knows what it can reach.
type SourceAdvisor interface {
	SourceEndpoints(context.Context) (hosts []string, port string)
}

var (
	probeHostShape = regexp.MustCompile(`^[A-Za-z0-9.:_-]{1,253}$`)
	probePortShape = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
)

const sourceProbeTimeout = 12 * time.Second

// sourceProbeScript builds the shell that tries each host from the target
// side. Every attempt runs in parallel and is cut off after four seconds,
// so an address that black-holes packets costs no more than a slow one.
// An attempt that finished on its own is reachable when its exit status is
// zero. The final "end" line proves the script ran to completion.
func sourceProbeScript(port string, hosts []string) (string, bool) {
	if len(hosts) == 0 || !probePortShape.MatchString(port) {
		return "", false
	}
	quoted := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if !probeHostShape.MatchString(host) {
			return "", false
		}
		quoted = append(quoted, "'"+host+"'")
	}
	var b strings.Builder
	hostList := strings.Join(quoted, " ")
	b.WriteString("port=" + port + "\ni=0\npids=''\n")
	// Each attempt is the very process blocked in connect(), so killing it
	// ends the attempt and closes its copy of stdout at once.
	b.WriteString("for host in " + hostList + "; do\n  ( exec 3<>\"/dev/tcp/$host/$port\" ) 2>/dev/null &\n  eval \"pid_$i=$!\"\n  pids=\"$pids $!\"\n  i=$((i+1))\ndone\n")
	// The window is measured by the shell's clock, so a sleep that refuses a
	// fractional argument only spins for the same four seconds.
	b.WriteString("started=$SECONDS\nwhile [ $((SECONDS-started)) -lt 4 ]; do\n  alive=0\n  for p in $pids; do kill -0 \"$p\" 2>/dev/null && alive=1; done\n  [ \"$alive\" = 0 ] && break\n  sleep 0.1\ndone\n")
	b.WriteString("i=0\nfor host in " + hostList + "; do\n  eval \"p=\\$pid_$i\"\n  if kill -0 \"$p\" 2>/dev/null; then\n    kill \"$p\" 2>/dev/null\n  elif wait \"$p\" 2>/dev/null; then\n    printf 'STEVE_REACH\\t%s\\t1\\n' \"$host\"\n  fi\n  i=$((i+1))\ndone\n")
	b.WriteString("wait 2>/dev/null\nprintf 'STEVE_REACH\\tend\\t1\\n'\n")
	return b.String(), true
}

// parseSourceProbe reads the target's answers. Without the end line the
// script was cut off, and silence about a host would be a false negative.
func parseSourceProbe(stdout string, hosts []string) ([]SourceHost, bool) {
	reached := map[string]bool{}
	ended := false
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 3 || fields[0] != "STEVE_REACH" || fields[2] != "1" {
			continue
		}
		if fields[1] == "end" {
			ended = true
			continue
		}
		reached[fields[1]] = true
	}
	if !ended {
		return nil, false
	}
	out := make([]SourceHost, 0, len(hosts))
	for _, host := range hosts {
		out = append(out, SourceHost{Host: host, Reachable: reached[host]})
	}
	return out, true
}

// probeSourceHosts records, on the check, which of this machine's addresses
// the target could open. A backend that does not need the answer, a
// target without bash, or a probe that did not finish leaves the check
// silent on the matter rather than wrong.
func (s *Service) probeSourceHosts(ctx context.Context, connection Connection, check *CheckResult) {
	advisor, ok := s.backend.(SourceAdvisor)
	if !ok || !check.HasTool("bash") {
		return
	}
	hosts, port := advisor.SourceEndpoints(ctx)
	script, ok := sourceProbeScript(port, hosts)
	if !ok {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, sourceProbeTimeout)
	defer cancel()
	output, err := connection.Run(probeCtx, "bash -s", script)
	if err != nil {
		return
	}
	if probed, ok := parseSourceProbe(output.Stdout, hosts); ok {
		check.SourceHosts = probed
	}
}
