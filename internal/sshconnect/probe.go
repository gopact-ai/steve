package sshconnect

import (
	"fmt"
	"net"
	"path"
	"strings"

	"github.com/gopact-ai/steve/internal/agenttools"
)

var probeTools = func() []string {
	tools := []string{"bash", "curl", "git", "nohup", "sha256sum", "shasum", "base64", "node", "npm"}
	for _, candidate := range agenttools.Catalog() {
		tools = append(tools, candidate.ID)
	}
	return tools
}()

// This input script has no user-supplied shell fragments and does not execute
// any discovered agent tool. POSIX utility lookups do not require installation.
var probeScript = `set -f
printf 'STEVE_CHECK\tos\t%s\n' "$(uname -s)"
printf 'STEVE_CHECK\tarch\t%s\n' "$(uname -m)"
printf 'STEVE_CHECK\tuser\t%s\n' "$(id -un)"
set -- $SSH_CONNECTION
printf 'STEVE_CHECK\taddress\t%s\n' "${3:-}"
for tool in ` + strings.Join(probeTools, " ") + `; do
  executable=$(command -v "$tool" 2>/dev/null || true)
  available=0
  case "$executable" in
    /*) if [ -f "$executable" ] && [ -x "$executable" ]; then available=1; fi ;;
  esac
  printf 'STEVE_CHECK\t%s\t%s\n' "$tool" "$available"
  if [ "$available" = 1 ]; then printf 'STEVE_CHECK\tpath_%s\t%s\n' "$tool" "$executable"; fi
done
existing=0
if [ -e "$HOME/steve-bin/node.json" ] || [ -L "$HOME/steve-bin/node.json" ] || [ -e "$HOME/.steve-node" ] || [ -L "$HOME/.steve-node" ] || [ -e "$HOME/.steve-peer" ] || [ -L "$HOME/.steve-peer" ]; then existing=1; fi
printf 'STEVE_CHECK\texisting\t%s\n' "$existing"
`

func parseProbe(output string) (map[string]string, error) {
	values := map[string]string{}
	allowed := map[string]bool{"os": true, "arch": true, "user": true, "address": true, "existing": true}
	for _, name := range probeTools {
		allowed[name] = true
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(fields) != 3 || fields[0] != "STEVE_CHECK" {
			continue
		}
		if !allowed[fields[1]] {
			if name, ok := strings.CutPrefix(fields[1], "path_"); ok && allowed[name] {
				if !path.IsAbs(fields[2]) || strings.ContainsAny(fields[2], "\x00\r\n") {
					return nil, fmt.Errorf("invalid tool path")
				}
				values[fields[1]] = fields[2]
			}
			continue
		}
		if _, found := values[fields[1]]; found {
			return nil, fmt.Errorf("duplicate probe field")
		}
		values[fields[1]] = fields[2]
	}
	for key := range allowed {
		value, found := values[key]
		if !found {
			return nil, fmt.Errorf("incomplete probe response")
		}
		switch key {
		case "os", "arch", "user":
			if value == "" || len(value) > 64 || strings.ContainsAny(value, "\x00\r\n") {
				return nil, fmt.Errorf("invalid platform field")
			}
		case "address":
			if value != "" && net.ParseIP(value) == nil {
				return nil, fmt.Errorf("invalid SSH address")
			}
		default:
			if value != "0" && value != "1" {
				return nil, fmt.Errorf("invalid probe boolean")
			}
		}
	}
	return values, nil
}
