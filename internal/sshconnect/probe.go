package sshconnect

import (
	"fmt"
	"net"
	"path"
	"regexp"
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

var existingProbePaths = []struct{ field, path string }{
	{"existing_path_node_config", "~/steve-bin/node.json"},
	{"existing_path_node_state", "~/.steve-node"},
	{"existing_path_peer_state", "~/.steve-peer"},
}

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
for entry in node_config node_state peer_state; do
  case "$entry" in
    node_config) location=steve-bin/node.json ;;
    node_state) location=.steve-node ;;
    peer_state) location=.steve-peer ;;
  esac
  if [ -e "$HOME/$location" ] || [ -L "$HOME/$location" ]; then
    existing=1
    printf 'STEVE_CHECK\texisting_path_%s\t~/%s\n' "$entry" "$location"
  fi
done
printf 'STEVE_CHECK\texisting\t%s\n' "$existing"
` + existingMetadataProbe

// Optional identity records help users locate the original registration. They
// are not proof of a running service or authenticated membership. Python's
// isolated mode avoids remote profiles/import hooks; reads remain fixed,
// bounded, and refuse symlinks beneath the account's canonical home without a
// stat/open race. Account home symlinks are common on managed hosts.
const existingMetadataProbe = `if [ "$existing" = 1 ]; then
  python=$(command -v python3 2>/dev/null || true)
  case "$python" in
    /*) if [ -f "$python" ] && [ -x "$python" ]; then
      "$python" -I -S -B - 2>/dev/null <<'STEVE_METADATA' || true
import json, os, re, stat

def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result

def recorded_value(directory, filename, key):
    descriptors = []
    try:
        directory_flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
        home = os.open(os.path.realpath(os.environ["HOME"]), directory_flags)
        descriptors.append(home)
        parent = os.open(directory, directory_flags, dir_fd=home)
        descriptors.append(parent)
        file = os.open(filename, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        descriptors.append(file)
        info = os.fstat(file)
        if not stat.S_ISREG(info.st_mode) or info.st_size > 65536:
            return
        with os.fdopen(os.dup(file), "rb") as stream:
            raw = stream.read(65537)
        if len(raw) > 65536:
            return
        data = json.loads(raw, object_pairs_hook=unique_object)
        value = data.get(key) if isinstance(data, dict) else None
        if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", value):
            return value
    except Exception:
        pass
    finally:
        for descriptor in reversed(descriptors):
            os.close(descriptor)

for field, directory, filename, key in (
    ("name", "steve-bin", "node.json", "name"),
    ("owner", ".steve-node", "hub.json", "hub"),
):
    value = recorded_value(directory, filename, key)
    if value is not None:
        print("STEVE_CHECK\texisting_node_" + field + "\t" + value)
STEVE_METADATA
    fi ;;
  esac
fi
`

var recordedIdentityShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func parseProbe(output string) (map[string]string, error) {
	values := map[string]string{}
	invalidMetadata := map[string]bool{}
	allowed := map[string]bool{"os": true, "arch": true, "user": true, "address": true, "existing": true}
	for _, name := range probeTools {
		allowed[name] = true
	}
	optionalPaths := map[string]string{}
	for _, entry := range existingProbePaths {
		optionalPaths[entry.field] = entry.path
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(fields) >= 2 && fields[0] == "STEVE_CHECK" && (fields[1] == "existing_node_name" || fields[1] == "existing_node_owner") {
			key := fields[1]
			if len(fields) != 3 || !recordedIdentityShape.MatchString(fields[2]) || values[key] != "" {
				invalidMetadata[key] = true
				delete(values, key)
			} else if !invalidMetadata[key] {
				values[key] = fields[2]
			}
			continue
		}
		if len(fields) >= 2 && fields[0] == "STEVE_CHECK" && strings.HasPrefix(fields[1], "existing_path_") && len(fields) != 3 {
			return nil, fmt.Errorf("invalid existing installation path")
		}
		if len(fields) != 3 || fields[0] != "STEVE_CHECK" {
			continue
		}
		if strings.HasPrefix(fields[1], "existing_path_") {
			expected, ok := optionalPaths[fields[1]]
			if !ok || fields[2] != expected {
				return nil, fmt.Errorf("invalid existing installation path")
			}
			if _, found := values[fields[1]]; found {
				return nil, fmt.Errorf("duplicate probe field")
			}
			values[fields[1]] = fields[2]
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
	for field := range optionalPaths {
		if values[field] != "" && values["existing"] != "1" {
			return nil, fmt.Errorf("inconsistent existing installation evidence")
		}
	}
	if values["existing"] != "1" {
		delete(values, "existing_node_name")
		delete(values, "existing_node_owner")
	}
	return values, nil
}
