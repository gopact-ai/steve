package nodebootstrap

import (
	"fmt"
	"regexp"
	"strings"
)

const PreviewUploadID = "pending-node-upload"

var uploadShape = regexp.MustCompile(`^[a-f0-9]{48}$`)

// UploadCommand receives raw bytes on stdin into an exclusive private staging
// directory. Only the server-generated plan ID enters the shell command.
func UploadCommand(id string) (string, error) {
	if !uploadShape.MatchString(id) {
		return "", fmt.Errorf("invalid upload ID")
	}
	script := fmt.Sprintf(`umask 077
if [ -e "$HOME/steve-bin/node.json" ] || [ -L "$HOME/steve-bin/node.json" ] || [ -e "$HOME/.steve-node" ] || [ -L "$HOME/.steve-node" ]; then
  echo 'An existing node configuration or state already exists.' >&2
  exit 20
fi
mkdir -p "$HOME/steve-bin"
upload_dir="$HOME/steve-bin/.upload-%s"
if ! mkdir "$upload_dir"; then
  echo 'The upload staging directory already exists.' >&2
  exit 21
fi
set -C
cat > "$upload_dir/steve-node"
`, id)
	return "sh -c " + shellQuote(script), nil
}

// CleanupUploadCommand removes only this plan's staging file and empty
// directory. It cannot delete an installed node, configuration or task state.
func CleanupUploadCommand(id string) (string, error) {
	if !uploadShape.MatchString(id) {
		return "", fmt.Errorf("invalid upload ID")
	}
	script := fmt.Sprintf(`upload_dir="$HOME/steve-bin/.upload-%s"
if [ -d "$upload_dir" ] && [ ! -L "$upload_dir" ]; then
  rm -f "$upload_dir/steve-node"
  rmdir "$upload_dir" 2>/dev/null || true
fi
`, id)
	return "sh -c " + shellQuote(script), nil
}

func shellQuote(text string) string { return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'" }
